// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

// Package progress renders optional terminal progress for long-running commands.
// It is the only package in the application that depends on mpb.
package progress

import (
	"context"
	"fmt"
	"io"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/mattn/go-runewidth"
	"github.com/vbauerster/mpb/v8"
	"github.com/vbauerster/mpb/v8/decor"
	"github.com/woozymasta/flags"
)

const (
	minimumTerminalWidth   = 64
	minimumLabelWidth      = 10
	minimumFillerWidth     = 10
	fixedCounterWidth      = 12
	fixedPercentageWidth   = 7
	fixedElapsedWidth      = 8
	barBoundariesWidth     = 2
	barSpacingWidth        = 2
	fixedRowWidth          = fixedCounterWidth + fixedPercentageWidth + barBoundariesWidth + barSpacingWidth
	overallMinimumCounter  = 4
	overallUnknownCounter  = 8
	overallPercentageWidth = 4
	overallPriority        = 0
	scopePriority          = 1
	detailPriorityOffset   = 2
	maximumLabelWidth      = 32
	maximumFillerWidth     = 100
	maximumBatchLabelWidth = 64
	maximumLayoutWidth     = fixedRowWidth + maximumLabelWidth + maximumFillerWidth
)

// Policy contains the runtime inputs used to decide whether terminal progress
// is safe and useful for the current process.
type Policy struct {
	// Stderr is the writer where progress would be rendered.
	Stderr io.Writer
	// LogOutput is the configured log destination, usually stderr or a file path.
	LogOutput string
	// LogFormat is the configured log encoding, such as text or json.
	LogFormat string
	// LogLevel is the configured minimum log level.
	LogLevel string
	// TerminalColumns is the width detected for the progress terminal.
	TerminalColumns int
	// NoProgress disables terminal progress explicitly.
	NoProgress bool
}

// Config controls one progress manager instance.
type Config struct {
	// Context provides values for progress rendering, but does not own the renderer lifecycle.
	// Close must finish the renderer after command cleanup
	// so logs cannot write to an already-cancelled mpb instance.
	Context context.Context
	// Output receives progress frames and progress-aware log records.
	Output io.Writer
	// Columns is the terminal width used to calculate the layout.
	Columns int
	// Enabled allows the manager to render after policy evaluation.
	Enabled bool
}

// Manager owns one mpb lifecycle for a command execution.
type Manager struct {
	output        io.Writer
	progress      *mpb.Progress
	finalMessages []string
	layout        Layout
	mutex         sync.Mutex
	pending       bool
	closed        bool
}

// Layout describes the fixed columns and available bar width for one terminal.
type Layout struct {
	// Columns is the rendered width of each progress row.
	Columns int
	// LabelWidth is the synchronized width reserved for bar labels.
	LabelWidth int
	// FillerWidth is the width reserved for the visual bar content.
	FillerWidth int
}

// Plan describes the semantic groups and scopes of one collection operation.
type Plan struct {
	// Scopes are namespaces or the cluster scope.
	Scopes []string
	// Jobs associates every resource job with one group and scope.
	Jobs []Job
}

// Job associates one collection unit with its display group and scope.
type Job struct {
	// Group is the resource display group handled by the job.
	Group string
	// Scope is a namespace or the literal cluster scope.
	Scope string
	// Failed reports that the job ended with an error.
	Failed bool
}

// Operation maps semantic collection events to synchronized progress bars.
type Operation struct {
	manager       *Manager
	overall       *mpb.Bar
	scopeBar      *mpb.Bar
	groups        map[string]*mpb.Bar
	groupPriority map[string]int
	groupJobs     map[string]int
	totals        map[string]int64
	scopes        map[string]*scopeState
	scopeLabel    string
	remainingJobs int
	overallTotal  int64
	mutex         sync.Mutex
	active        bool
	failed        bool
	closed        bool
}

// scopeState tracks unfinished jobs so a scope bar advances once per scope,
// rather than once per individual resource request.
type scopeState struct {
	remaining int
}

// ShouldEnable reports whether progress can be rendered without corrupting
// command output or making non-interactive logs noisy.
func ShouldEnable(policy Policy) bool {
	if policy.NoProgress || policy.Stderr == nil {
		return false
	}

	file, ok := policy.Stderr.(*os.File)
	if !ok || !flags.DetectFileTTY(file) {
		return false
	}

	if policy.TerminalColumns < minimumTerminalWidth {
		return false
	}

	logsOnTerminal := policy.LogOutput == "" || policy.LogOutput == "stderr"
	if logsOnTerminal && policy.LogFormat == "json" {
		return false
	}
	if logsOnTerminal && (policy.LogLevel == "debug" || policy.LogLevel == "trace") {
		return false
	}

	return true
}

// New creates a progress manager.
// It returns a no-op manager when rendering is disabled
// or the terminal cannot fit the configured layout.
func New(config Config) *Manager {
	layout, ok := calculateLayout(config.Columns)
	manager := &Manager{output: config.Output}
	if !config.Enabled || !ok || config.Output == nil {
		return manager
	}

	ctx := context.Background()
	if config.Context != nil {
		// The command context is cancelled by Ctrl+C before command cleanup completes.
		// Detach mpb from that cancellation;
		// Manager.Close is the synchronization point that shuts down both progress and its writer.
		ctx = context.WithoutCancel(config.Context)
	}

	manager.layout = layout
	manager.progress = mpb.NewWithContext(
		ctx,
		mpb.WithOutput(config.Output),
		mpb.WithWidth(layout.Columns),
		mpb.WithAutoRefresh(),
	)

	return manager
}

// Enabled reports whether this manager owns a live terminal renderer.
func (m *Manager) Enabled() bool {
	if m == nil {
		return false
	}

	m.mutex.Lock()
	defer m.mutex.Unlock()

	return m.progress != nil && !m.closed
}

// Columns reports the terminal width used by this manager.
// It returns zero when progress rendering is disabled.
func (m *Manager) Columns() int {
	if m == nil || !m.Enabled() {
		return 0
	}

	return m.layout.Columns
}

// LogWriter returns a writer that lets mpb redraw bars around log records.
// A disabled manager returns nil because callers
// should keep the logger's normal output sink in that case.
func (m *Manager) LogWriter() io.Writer {
	if !m.Enabled() {
		return nil
	}

	return progressLogWriter{manager: m}
}

// Close finishes progress after command execution.
// Presentation errors are intentionally ignored;
// they must never replace the command's real result.
func (m *Manager) Close(commandErr error) {
	if m == nil {
		return
	}

	m.mutex.Lock()
	if m.progress == nil || m.closed {
		m.mutex.Unlock()
		return
	}

	// Stop routing new records through mpb before shutting it down.
	// Late records from cancellation cleanup are written directly by Write.
	m.closed = true
	pending := m.pending
	m.pending = false
	progress := m.progress
	m.mutex.Unlock()

	if pending {
		m.flushPendingLogs(progress)
	}
	finalMessages := m.takeFinalMessages()
	if commandErr != nil {
		progress.Shutdown()
	} else {
		progress.Wait()
	}

	for _, message := range finalMessages {
		_, _ = fmt.Fprintln(m.output, message)
	}
}

// progressLogWriter routes logger output through mpb and records that a render
// may be needed before the progress container shuts down.
type progressLogWriter struct {
	manager *Manager
}

// Write implements io.Writer for progress-aware logger output.
func (w progressLogWriter) Write(data []byte) (int, error) {
	if w.manager == nil {
		return 0, io.ErrClosedPipe
	}

	w.manager.mutex.Lock()
	defer w.manager.mutex.Unlock()
	if w.manager.progress == nil {
		return 0, io.ErrClosedPipe
	}
	if w.manager.closed {
		return w.manager.output.Write(data)
	}

	n, err := w.manager.progress.Write(data)
	if n > 0 {
		w.manager.pending = true
	}

	return n, err
}

// AddFinalMessage queues one human-readable summary for output after progress rendering has finished
// It is intentionally separate from logger output
// so completion summaries cannot appear above active progress bars.
func (m *Manager) AddFinalMessage(message string) {
	if m == nil || !m.Enabled() || strings.TrimSpace(message) == "" {
		return
	}

	m.mutex.Lock()
	m.finalMessages = append(m.finalMessages, message)
	m.mutex.Unlock()
}

// flushPendingLogs adds a non-rendering completion marker
// when logger output was buffered after the last regular progress refresh.
// The marker is removed by mpb and exists only to make its final render flush pending warnings
// or errors without exposing another progress row.
func (m *Manager) flushPendingLogs(progress *mpb.Progress) {
	if m == nil || progress == nil {
		return
	}

	marker := progress.New(1, mpb.NopStyle(), mpb.BarRemoveOnComplete())
	marker.Increment()
}

// takeFinalMessages removes queued summaries for one renderer lifecycle.
func (m *Manager) takeFinalMessages() []string {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	messages := m.finalMessages
	m.finalMessages = nil
	return messages
}

// NewOperation creates a collection progress operation on this manager.
func (m *Manager) NewOperation() *Operation {
	return &Operation{
		manager:       m,
		groups:        make(map[string]*mpb.Bar),
		groupPriority: make(map[string]int),
		groupJobs:     make(map[string]int),
		totals:        make(map[string]int64),
		scopes:        make(map[string]*scopeState),
	}
}

// Counter tracks one known-size operation with a single terminal bar.
// It is intended for commands whose work has no Kubernetes resource and scope hierarchy,
// such as copying image-layout files or PVC artifacts.
type Counter struct {
	bar      *mpb.Bar
	label    *batchItemStatus
	keepOpen bool
	mutex    sync.Mutex
	total    int64
	current  int64
}

// Batch tracks a fixed set of independent countable items.
// It renders one aggregate bar and one percentage-only bar per item.
type Batch struct {
	overall   *mpb.Bar
	items     map[string]*mpb.Bar
	completed map[string]struct{}
	progress  map[string]int
	statuses  map[string]*batchItemStatus
	mutex     sync.Mutex
	failed    bool
	closed    bool
}

// batchItemStatus stores the mutable label shown for one batch item.
type batchItemStatus struct {
	label string
	stage string
	mutex sync.RWMutex
}

// display returns the current stage before the item identity
// so important lifecycle information remains visible even when the terminal truncates it.
func (s *batchItemStatus) display() string {
	s.mutex.RLock()
	defer s.mutex.RUnlock()

	if s.stage == "" {
		return s.label
	}

	return s.stage + ": " + s.label
}

// NewCounter creates a single progress bar for a countable operation.
// A non-positive total returns a no-op counter
// because there is no useful progress to display for an empty input.
func (m *Manager) NewCounter(label string, total int64) *Counter {
	counter := &Counter{}
	if m == nil || !m.Enabled() || total <= 0 {
		return counter
	}

	counter.bar = m.newOverallBar(label, total)
	counter.total = total
	return counter
}

// NewCurrentPercentCounter creates one reusable detail bar for sequential items.
// The row remains visible while the caller resets its label for the next item.
func (m *Manager) NewCurrentPercentCounter(label string) *Counter {
	counter := &Counter{
		label:    &batchItemStatus{label: label},
		keepOpen: true,
		total:    100,
	}
	if m == nil || !m.Enabled() {
		return counter
	}

	layout, valid := calculateBatchLayout(m.layout.Columns)
	if !valid {
		return counter
	}

	counter.bar = m.newPercentBarFuncWithLayout(
		layout,
		counter.label.display,
		100,
		mpb.BarPriority(detailPriorityOffset),
	)

	return counter
}

// Reset starts the reusable detail bar from zero with a new item label.
func (c *Counter) Reset(label string) {
	if c == nil || c.bar == nil {
		return
	}

	c.SetLabel(label)
	c.mutex.Lock()
	c.current = 0
	c.total = 100
	c.mutex.Unlock()
	c.bar.SetTotal(100, false)
	c.bar.SetCurrent(0)
}

// SetLabel changes the label of a reusable detail bar.
func (c *Counter) SetLabel(label string) {
	if c == nil || c.label == nil {
		return
	}

	c.label.mutex.Lock()
	c.label.label = label
	c.label.stage = ""
	c.label.mutex.Unlock()
}

// NewBatch creates a progress layout for a fixed set of items.
// Empty item lists return a no-op batch, so commands never render meaningless 0/0 rows.
func (m *Manager) NewBatch(label string, items []string) *Batch {
	batch := &Batch{
		items:     make(map[string]*mpb.Bar),
		completed: make(map[string]struct{}),
		progress:  make(map[string]int),
		statuses:  make(map[string]*batchItemStatus),
	}
	if m == nil || !m.Enabled() {
		return batch
	}

	items = uniqueSorted(items)
	if len(items) == 0 {
		return batch
	}
	batchLayout, _ := calculateBatchLayout(m.layout.Columns)

	batch.overall = m.newOverallBar(label, int64(len(items)), mpb.BarPriority(overallPriority))
	for index, item := range items {
		status := &batchItemStatus{label: item}
		batch.statuses[item] = status
		batch.items[item] = m.newPercentBarFuncWithLayout(
			batchLayout,
			status.display,
			100,
			mpb.BarPriority(detailPriorityOffset+index),
		)
	}

	return batch
}

// Complete marks one batch item as successfully processed.
func (b *Batch) Complete(item string) {
	b.SetStage(item, "done")
	b.finish(item, false)
}

// Enabled reports whether this batch owns visible progress bars.
func (b *Batch) Enabled() bool {
	return b != nil && len(b.items) > 0
}

// Fail marks one batch item as failed and stops its bar.
func (b *Batch) Fail(item string) {
	b.SetStage(item, "failed")
	b.finish(item, true)
}

// SetProgress updates one item to an absolute lifecycle percentage.
// The value is monotonic and is intentionally independent of the number of bytes in the PVC
// because the stream size is not known before export starts.
func (b *Batch) SetProgress(item string, percent int) {
	if b == nil {
		return
	}
	percent = max(min(percent, 100), 0)

	b.mutex.Lock()
	bar, known := b.items[item]
	if !known || b.closed || percent <= b.progress[item] {
		b.mutex.Unlock()
		return
	}

	delta := percent - b.progress[item]
	b.progress[item] = percent
	b.mutex.Unlock()

	bar.IncrBy(delta)
}

// SetStage changes the human-readable lifecycle stage
// of one item without affecting its numeric progress.
func (b *Batch) SetStage(item, stage string) {
	if b == nil {
		return
	}

	b.mutex.Lock()
	status := b.statuses[item]
	b.mutex.Unlock()
	if status == nil {
		return
	}

	status.mutex.Lock()
	status.stage = stage
	status.mutex.Unlock()
}

// Close finalizes all batch bars and prevents mpb from waiting for unfinished items.
func (b *Batch) Close(commandErr error) {
	if b == nil {
		return
	}

	b.mutex.Lock()
	if b.closed {
		b.mutex.Unlock()
		return
	}

	b.closed = true
	overall := b.overall
	items := make([]*mpb.Bar, 0, len(b.items))
	for item, bar := range b.items {
		if _, complete := b.completed[item]; !complete {
			items = append(items, bar)
		}
	}
	failure := commandErr != nil || b.failed || len(items) > 0
	b.mutex.Unlock()

	for _, bar := range items {
		if failure {
			bar.Abort(false)
		} else {
			bar.SetTotal(100, true)
		}
	}
	if overall == nil {
		return
	}

	if failure {
		overall.Abort(false)
	} else {
		overall.SetTotal(int64(len(b.items)), true)
	}
}

// finish applies one terminal item update exactly once.
func (b *Batch) finish(item string, failed bool) {
	if b == nil {
		return
	}

	b.mutex.Lock()
	bar, known := b.items[item]
	if !known || b.closed {
		b.mutex.Unlock()
		return
	}
	if _, complete := b.completed[item]; complete {
		b.mutex.Unlock()
		return
	}

	b.completed[item] = struct{}{}
	if failed {
		b.failed = true
	}
	overall := b.overall
	b.mutex.Unlock()

	if failed {
		bar.Abort(false)
		return
	}

	b.SetProgress(item, 100)
	bar.SetTotal(100, true)
	if overall != nil {
		overall.Increment()
	}
}

// Increment advances the counter after one item has been processed.
func (c *Counter) Increment() {
	if c == nil || c.bar == nil {
		return
	}

	c.mutex.Lock()
	c.current++
	c.mutex.Unlock()
	c.bar.Increment()
}

// Add advances the counter by a number of units, such as bytes read from an archive.
// Non-positive deltas are ignored because they cannot represent work.
func (c *Counter) Add(delta int64) {
	if c == nil || c.bar == nil || delta <= 0 {
		return
	}

	c.mutex.Lock()
	c.current += delta
	c.mutex.Unlock()
	maxInt := int64(^uint(0) >> 1)
	for delta > maxInt {
		c.bar.IncrBy(int(maxInt))
		delta -= maxInt
	}
	c.bar.IncrBy(int(delta))
}

// SetProgress updates a reusable detail bar to an absolute percentage.
// The value is monotonic because progress callbacks can arrive out of order.
func (c *Counter) SetProgress(total, current int64) {
	if c == nil || c.bar == nil || total <= 0 || current < 0 {
		return
	}
	if current > total {
		current = total
	}

	c.mutex.Lock()
	if total < c.total {
		total = c.total
	}
	if current < c.current {
		current = c.current
	}
	delta := current - c.current
	c.total = total
	c.current = current
	c.mutex.Unlock()

	c.bar.SetTotal(total, false)
	if delta > 0 {
		maxInt := int64(^uint(0) >> 1)
		for delta > maxInt {
			c.bar.IncrBy(int(maxInt))
			delta -= maxInt
		}
		c.bar.IncrBy(int(delta))
	}
	if current >= total && !c.keepOpen {
		c.bar.SetTotal(total, true)
	}
}

// Complete marks the counter complete even when the operation stopped after a partial input,
// so the renderer does not wait for a bar that cannot advance.
func (c *Counter) Complete() {
	if c == nil || c.bar == nil {
		return
	}

	c.mutex.Lock()
	total := c.total
	if total == 0 {
		total = c.current
	}
	c.total = total
	c.mutex.Unlock()
	c.bar.SetTotal(total, true)
}

// Abort removes a counter that could not complete.
func (c *Counter) Abort() {
	if c == nil || c.bar == nil {
		return
	}

	c.bar.Abort(true)
}

// TrackReader wraps a reader and reports bytes returned by successful reads.
// It leaves the source untouched when the counter is a no-op.
func TrackReader(reader io.Reader, counter *Counter) io.Reader {
	if reader == nil || counter == nil || counter.bar == nil {
		return reader
	}

	return &countingReader{reader: reader, counter: counter}
}

// countingReader reports input bytes without changing reader semantics.
type countingReader struct {
	reader  io.Reader
	counter *Counter
}

// Read forwards one read and records only bytes actually returned by the source.
func (r *countingReader) Read(buffer []byte) (int, error) {
	read, err := r.reader.Read(buffer)
	r.counter.Add(int64(read))

	return read, err
}

// SetPlan registers collection jobs and scopes before workers start.
// Sorting here keeps scope order stable even when Kubernetes discovery or workers complete out of order.
// Resource bars are still created lazily
// because a planned resource can have no objects in the selected scope.
func (o *Operation) SetPlan(plan Plan) {
	if o == nil || o.manager == nil || !o.manager.Enabled() {
		return
	}

	scopes := uniqueSorted(plan.Scopes)
	jobsByScope := make(map[string]int)

	o.mutex.Lock()
	defer o.mutex.Unlock()

	if o.active {
		return
	}
	o.active = true
	o.remainingJobs = len(plan.Jobs)

	for _, job := range plan.Jobs {
		jobsByScope[job.Scope]++
		o.groupJobs[job.Group]++
		if _, exists := o.groupPriority[job.Group]; !exists {
			o.groupPriority[job.Group] = len(o.groupPriority) + 1
		}
	}

	if len(scopes) > 0 {
		o.scopeLabel = "Namespaces"
		if slices.Contains(scopes, "cluster") {
			o.scopeLabel = "Scopes"
		}

		for _, scope := range scopes {
			o.scopes[scope] = &scopeState{remaining: jobsByScope[scope]}
		}
	}
}

// AddTotal adds listed objects to a resource group and to the overall denominator.
func (o *Operation) AddTotal(group string, total int) {
	if o == nil || o.manager == nil || !o.manager.Enabled() || total < 0 {
		return
	}

	o.mutex.Lock()
	o.totals[group] += int64(total)
	groupTotal := o.totals[group]
	o.overallTotal += int64(total)
	bar := o.groups[group]

	// Create the aggregate first so mpb keeps it above all detail bars.
	if total > 0 && o.overall == nil {
		o.overall = o.manager.newOverallBar("All objects", -1, mpb.BarPriority(overallPriority))
	}

	// Create scopes after the aggregate and before resource detail bars.
	if total > 0 && o.scopeBar == nil && len(o.scopes) > 0 {
		o.scopeBar = o.manager.newBar(
			o.scopeLabel,
			int64(len(o.scopes)),
			mpb.BarPriority(scopePriority),
		)
	}

	// Create a detail bar with the accumulated total.
	// Empty groups never create a bar, while groups spanning several namespaces
	// can extend this total before their final job is reported.
	if total > 0 && o.overall != nil {
		if o.groups[group] == nil {
			o.groups[group] = o.manager.newBar(
				group,
				-1,
				mpb.BarPriority(detailPriorityOffset+o.groupPriority[group]),
			)
		}

		bar = o.groups[group]
	}

	overall := o.overall
	overallTotal := o.overallTotal
	o.mutex.Unlock()

	if bar != nil {
		bar.SetTotal(groupTotal, false)
	}
	if overall != nil {
		overall.SetTotal(overallTotal, false)
	}
}

// Increment marks one listed object as processed by a resource group and overall.
func (o *Operation) Increment(group string) {
	if o == nil {
		return
	}

	o.mutex.Lock()

	bar := o.groups[group]
	overall := o.overall

	o.mutex.Unlock()

	if bar != nil {
		bar.Increment()
	}
	if overall != nil {
		overall.Increment()
	}
}

// FinishJob records a collection job result, completes a resource group after its final job,
// and advances its scope after all jobs in that scope finish.
func (o *Operation) FinishJob(job Job) {
	if o == nil {
		return
	}

	o.mutex.Lock()

	scope := o.scopes[job.Scope]
	group := o.groups[job.Group]
	scopeBar := o.scopeBar
	groupTotal := o.totals[job.Group]
	if job.Failed {
		o.failed = true
	}
	if o.groupJobs[job.Group] > 0 {
		o.groupJobs[job.Group]--
	}
	groupDone := o.groupJobs[job.Group] == 0
	overallDone := false

	scopeDone := false
	if scope != nil && scope.remaining > 0 {
		scope.remaining--
		scopeDone = scope.remaining == 0
	}
	if o.remainingJobs > 0 {
		o.remainingJobs--
		overallDone = o.remainingJobs == 0
	}
	overall := o.overall
	overallTotal := o.overallTotal
	failed := o.failed

	o.mutex.Unlock()

	if group != nil && groupDone && groupTotal > 0 {
		group.SetTotal(groupTotal, true)
	}
	if overallDone && overall != nil {
		if failed {
			overall.Abort(false)
		} else {
			overall.SetTotal(overallTotal, true)
		}
	}
	if scopeDone && scopeBar != nil {
		scopeBar.Increment()
	}
}

// Close finalizes the operation after the collection pipeline has stopped.
// It sends only terminal state updates to mpb;
// querying bar state here can deadlock with queued increments when a fast namespace-scoped run completes.
func (o *Operation) Close() {
	if o == nil {
		return
	}

	o.mutex.Lock()
	if o.closed {
		o.mutex.Unlock()
		return
	}

	o.closed = true
	type finalBar struct {
		bar   *mpb.Bar
		total int64
	}

	bars := make([]finalBar, 0, len(o.groups))
	for group, bar := range o.groups {
		bars = append(bars, finalBar{bar: bar, total: o.totals[group]})
	}

	overall := o.overall
	overallTotal := o.overallTotal
	scopeBar := o.scopeBar
	scopeTotal := int64(len(o.scopes))
	failed := o.failed || o.remainingJobs > 0
	o.mutex.Unlock()

	for _, item := range bars {
		if failed {
			item.bar.Abort(false)
			continue
		}

		item.bar.SetTotal(item.total, true)
	}

	if overall != nil {
		if failed {
			overall.Abort(false)
		} else {
			overall.SetTotal(overallTotal, true)
		}
	}

	if scopeBar != nil {
		if failed {
			scopeBar.Abort(false)
		} else {
			scopeBar.SetTotal(scopeTotal, true)
		}
	}
}

// calculateLayout rejects terminals where labels and counters would collide.
// On wide terminals it caps the filler instead of stretching the bar across the entire screen,
// keeping progress rows visually compact.
func calculateLayout(columns int) (Layout, bool) {
	if columns < minimumTerminalWidth {
		return Layout{}, false
	}

	fixed := fixedRowWidth
	labelWidth := min(maximumLabelWidth, columns-fixed-minimumFillerWidth)
	if labelWidth < minimumLabelWidth {
		return Layout{}, false
	}

	fillerWidth := min(columns-fixed-labelWidth, maximumFillerWidth)
	renderedColumns := fixed + labelWidth + fillerWidth

	return Layout{
		Columns:     renderedColumns,
		LabelWidth:  labelWidth,
		FillerWidth: fillerWidth,
	}, true
}

// calculateOverallLayout gives aggregate bars a compact label
// and counter layout while preserving the manager's full row width.
// The shorter decorators are compensated by a wider filler
// so aggregate, detail, and status rows align.
func calculateOverallLayout(base Layout, label string, counterWidth int) Layout {
	labelWidth := max(runewidth.StringWidth(label)+barSpacingWidth, 1)
	fixedWidth := counterWidth + overallPercentageWidth + barBoundariesWidth + barSpacingWidth
	fillerWidth := base.Columns - labelWidth - fixedWidth
	if fillerWidth < minimumFillerWidth {
		return base
	}

	return Layout{
		Columns:     base.Columns,
		LabelWidth:  labelWidth,
		FillerWidth: fillerWidth,
	}
}

// calculateOverallCounterWidth reserves only the digits required by a known aggregate total.
// Unknown totals keep the wider legacy-safe column.
func calculateOverallCounterWidth(total int64) int {
	if total < 0 {
		return overallUnknownCounter
	}

	width := runewidth.StringWidth(fmt.Sprintf("%d/%d ", total, total))
	if width < overallMinimumCounter {
		return overallMinimumCounter
	}

	return width
}

// calculateBatchLayout reserves more room for namespaced PVC identities
// while keeping the visual bar compact enough to distinguish individual rows.
func calculateBatchLayout(columns int) (Layout, bool) {
	if columns < minimumTerminalWidth {
		return Layout{}, false
	}

	fixed := fixedRowWidth
	labelWidth := min(maximumBatchLabelWidth, columns-fixed-minimumFillerWidth)
	if labelWidth < minimumLabelWidth {
		return Layout{}, false
	}

	// Detail rows use the same rendered width as aggregate rows.
	// Keeping the full remainder for the filler aligns the right edge of bars,
	// counters, percentages, and the separator even when the label column is wider.
	fillerWidth := columns - fixed - labelWidth

	return Layout{
		Columns:     columns,
		LabelWidth:  labelWidth,
		FillerWidth: fillerWidth,
	}, true
}

// TruncateLabel preserves the beginning of a label and reserves space for an ellipsis.
func TruncateLabel(label string, width int) string {
	if width <= 0 {
		return ""
	}

	return runewidth.Truncate(label, width, "...")
}

// newBar creates the common mpb style and synchronized decorator columns.
func (m *Manager) newBar(label string, total int64, options ...mpb.BarOption) *mpb.Bar {
	return m.newBarWithLayout(m.layout, label, total, options...)
}

// newOverallBar creates an aggregate bar with a compact label column
// and a status divider that has exactly the same rendered width as the bar row.
func (m *Manager) newOverallBar(label string, total int64, options ...mpb.BarOption) *mpb.Bar {
	counterWidth := calculateOverallCounterWidth(total)
	layout := calculateOverallLayout(m.layout, label, counterWidth)
	options = append(options, mpb.BarBtmExtender(newStatusFiller(layout.Columns)))

	return m.newBarWithLayoutAndDecorators(
		layout,
		label,
		total,
		counterWidth,
		overallPercentageWidth,
		options...,
	)
}

// newBarWithLayout creates a progress bar using a caller-selected column layout.
func (m *Manager) newBarWithLayout(layout Layout, label string, total int64, options ...mpb.BarOption) *mpb.Bar {
	return m.newBarWithLayoutAndDecorators(
		layout,
		label,
		total,
		fixedCounterWidth,
		fixedPercentageWidth,
		options...,
	)
}

// newBarWithLayoutAndDecorators creates a progress bar with explicit counter widths
// so aggregate and detail rows can use different compact layouts.
func (m *Manager) newBarWithLayoutAndDecorators(
	layout Layout,
	label string,
	total int64,
	counterWidth int,
	percentageWidth int,
	options ...mpb.BarOption,
) *mpb.Bar {
	barOptions := make([]mpb.BarOption, 0, 2+len(options))
	barOptions = append(barOptions,
		mpb.PrependDecorators(
			decor.Name(
				TruncateLabel(label, layout.LabelWidth),
				decor.WC{W: layout.LabelWidth, C: decor.DindentRight},
			),
		),
		mpb.AppendDecorators(
			decor.CountersNoUnit(
				"%d/%d ",
				decor.WC{W: counterWidth},
			),
			decor.NewPercentage(
				"%d",
				decor.WC{W: percentageWidth},
			),
		),
	)
	barOptions = append(barOptions, options...)
	barOptions = append(barOptions, mpb.BarWidth(layout.FillerWidth+barBoundariesWidth))

	return m.progress.New(
		total,
		mpb.BarStyle().
			Lbound("[").
			Filler("=").
			Tip(">").
			Padding("-").
			Rbound("]"),
		barOptions...,
	)
}

// newPercentBarFuncWithLayout creates a percentage-only bar using a caller-selected layout.
func (m *Manager) newPercentBarFuncWithLayout(
	layout Layout,
	label func() string,
	total int64,
	options ...mpb.BarOption,
) *mpb.Bar {
	barOptions := make([]mpb.BarOption, 0, 3+len(options))
	barOptions = append(barOptions,
		mpb.PrependDecorators(
			decor.Any(
				func(decor.Statistics) string {
					return TruncateLabel(label(), layout.LabelWidth)
				},
				decor.WC{W: layout.LabelWidth, C: decor.DindentRight},
			),
			decor.Name("", decor.WC{W: fixedCounterWidth}),
		),
		mpb.AppendDecorators(
			decor.NewPercentage(
				"%d",
				decor.WC{W: fixedPercentageWidth},
			),
		),
	)
	barOptions = append(barOptions, options...)
	barOptions = append(barOptions, mpb.BarWidth(layout.FillerWidth+barBoundariesWidth))

	return m.progress.New(
		total,
		mpb.BarStyle().
			Lbound("[").
			Filler("=").
			Tip(">").
			Padding("-").
			Rbound("]"),
		barOptions...,
	)
}

// newStatusFiller creates a compact time status row below the overall bar
// with the same width as the surrounding progress rows.
// It is an extender of that bar rather than another progress bar,
// so it cannot change ordering, totals, completion, or the number of tracked operations.
func newStatusFiller(requestedWidth int) mpb.BarFiller {
	started := time.Now()
	frames := []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

	return mpb.BarFillerFunc(func(w io.Writer, stat decor.Statistics) error {
		width := requestedWidth
		if width <= 0 {
			width = stat.AvailableWidth
		}
		if width <= 0 {
			width = stat.RequestedWidth
		}

		elapsed := time.Since(started)
		frame := frames[int(elapsed/(150*time.Millisecond))%len(frames)]
		if stat.Completed {
			frame = "✓"
		} else if stat.Aborted {
			frame = "✗"
		}

		hours := min(int(elapsed/time.Hour), 99)
		elapsedText := fmt.Sprintf("%02d:%02d:%02d", hours, int(elapsed/time.Minute)%60, int(elapsed/time.Second)%60)
		status := fmt.Sprintf(" %s %s %s ", frame, elapsedText, frame)
		padding := max(width-runewidth.StringWidth(status), 0)
		leftPadding := padding / 2
		rightPadding := padding - leftPadding
		divider := strings.Repeat("─", leftPadding) + status + strings.Repeat("─", rightPadding)
		_, err := fmt.Fprintln(w, divider)

		return err
	})
}

// uniqueSorted removes duplicate display entries and returns deterministic order.
func uniqueSorted(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}

	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)

	return result
}
