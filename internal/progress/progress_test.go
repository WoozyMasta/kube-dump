// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package progress

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/mattn/go-runewidth"
	"github.com/vbauerster/mpb/v8/decor"
)

func TestCalculateLayout(t *testing.T) {
	tests := []struct {
		name    string
		columns int
		valid   bool
	}{
		{name: "too narrow", columns: minimumTerminalWidth - 1},
		{name: "minimum", columns: minimumTerminalWidth, valid: true},
		{name: "wide", columns: 120, valid: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			layout, valid := calculateLayout(test.columns)
			if valid != test.valid {
				t.Fatalf("calculateLayout(%d) valid = %t, want %t", test.columns, valid, test.valid)
			}
			if !valid {
				return
			}

			if layout.LabelWidth < minimumLabelWidth || layout.FillerWidth < minimumFillerWidth {
				t.Fatalf("calculateLayout(%d) = %#v, minimum columns are too small", test.columns, layout)
			}
			wantColumns := min(test.columns, maximumLayoutWidth)
			if layout.Columns != wantColumns {
				t.Fatalf("calculateLayout(%d) = %#v, rendered width = %d, want %d", test.columns, layout, layout.Columns, wantColumns)
			}
			if layout.LabelWidth+layout.FillerWidth+fixedRowWidth != layout.Columns {
				t.Fatalf("calculateLayout(%d) = %#v, columns do not add up", test.columns, layout)
			}
		})
	}

	layout, valid := calculateLayout(maximumLayoutWidth + 80)
	if !valid || layout.FillerWidth != maximumFillerWidth {
		t.Fatalf("wide calculateLayout() = %#v, want filler width %d", layout, maximumFillerWidth)
	}
}

func TestCalculateOverallLayoutUsesCompactLabel(t *testing.T) {
	base, valid := calculateLayout(200)
	if !valid {
		t.Fatal("calculateLayout() rejected a valid terminal width")
	}

	counterWidth := calculateOverallCounterWidth(2)
	layout := calculateOverallLayout(base, "PVCs", counterWidth)
	if layout.LabelWidth != len("PVCs")+barSpacingWidth {
		t.Fatalf("calculateOverallLayout() label width = %d, want %d", layout.LabelWidth, len("PVCs")+barSpacingWidth)
	}

	wantFillerWidth := base.Columns - layout.LabelWidth - counterWidth - overallPercentageWidth -
		barBoundariesWidth - barSpacingWidth
	if layout.FillerWidth != wantFillerWidth {
		t.Fatalf(
			"calculateOverallLayout() filler width = %d, want %d",
			layout.FillerWidth, wantFillerWidth)
	}
	if layout.Columns != base.Columns {
		t.Fatalf("calculateOverallLayout() columns = %d, want %d", layout.Columns, base.Columns)
	}
	if layout.LabelWidth+layout.FillerWidth+counterWidth+overallPercentageWidth+
		barBoundariesWidth+barSpacingWidth != layout.Columns {
		t.Fatalf("calculateOverallLayout() = %#v, columns do not add up", layout)
	}
}

func TestCalculateOverallCounterWidth(t *testing.T) {
	tests := []struct {
		total int64
		want  int
	}{
		{total: -1, want: overallUnknownCounter},
		{total: 2, want: len("2/2 ")},
		{total: 721, want: len("721/721 ")},
	}

	for _, test := range tests {
		if got := calculateOverallCounterWidth(test.total); got != test.want {
			t.Errorf("calculateOverallCounterWidth(%d) = %d, want %d", test.total, got, test.want)
		}
	}
}

func TestCalculateBatchLayout(t *testing.T) {
	layout, valid := calculateBatchLayout(120)
	if !valid {
		t.Fatal("calculateBatchLayout() rejected a valid terminal width")
	}
	if layout.LabelWidth != maximumBatchLabelWidth || layout.FillerWidth != 33 {
		t.Fatalf("calculateBatchLayout(120) = %#v, want label 64 and filler 33", layout)
	}
	if layout.LabelWidth+layout.FillerWidth+fixedRowWidth != layout.Columns {
		t.Fatalf("calculateBatchLayout(120) = %#v, columns do not add up", layout)
	}

	layout, valid = calculateBatchLayout(200)
	if !valid || layout.LabelWidth != maximumBatchLabelWidth || layout.FillerWidth != 113 {
		t.Fatalf("calculateBatchLayout(200) = %#v, want shared-width layout", layout)
	}
}

func TestTruncateLabel(t *testing.T) {
	if got := TruncateLabel("gateway.networking.k8s.io", 12); got != "gateway.n..." {
		t.Fatalf("TruncateLabel() = %q, want %q", got, "gateway.n...")
	}
	if got := TruncateLabel("short", 12); got != "short" {
		t.Fatalf("TruncateLabel() changed a short label to %q", got)
	}
}

func TestBatchItemDisplaySeparatesStageAndIdentity(t *testing.T) {
	status := &batchItemStatus{
		label: "alloy-metrics/data",
		stage: "snapshot pending",
	}

	if got := status.display(); got != "snapshot pending: alloy-metrics/data" {
		t.Fatalf("batch item display = %q", got)
	}
}

func TestStatusFillerIsOneNonProgressRow(t *testing.T) {
	const width = 80

	var output bytes.Buffer
	if err := newStatusFiller(width).Fill(&output, decor.Statistics{AvailableWidth: width + 80}); err != nil {
		t.Fatalf("newStatusFiller().Fill() error = %v", err)
	}

	line := strings.TrimSuffix(output.String(), "\n")
	fields := strings.Fields(line)
	if len(fields) != 5 || fields[1] != fields[3] || len(fields[2]) != len("00:00:00") {
		t.Fatalf("status filler = %q, want divider around '<spinner> HH:MM:SS <spinner>'", output.String())
	}
	if !strings.ContainsRune(line, '─') {
		t.Fatalf("status filler = %q, want divider characters", output.String())
	}
	if runewidth.StringWidth(line) != width {
		t.Fatalf("status filler width = %d, want %d", runewidth.StringWidth(line), width)
	}
}

func TestLogWriterFlushesAfterProgressCompletes(t *testing.T) {
	var output bytes.Buffer
	manager := New(Config{Output: &output, Columns: 120, Enabled: true})
	if !manager.Enabled() {
		t.Fatal("New() disabled progress manager")
	}

	counter := manager.NewCounter("Images", 1)
	counter.Increment()
	if _, err := manager.LogWriter().Write([]byte("summary\n")); err != nil {
		t.Fatalf("LogWriter().Write() error = %v", err)
	}
	manager.Close(nil)

	if !strings.Contains(output.String(), "summary") {
		t.Fatalf("pending log was not flushed: %q", output.String())
	}
}

func TestFinalMessageIsPrintedAfterProgress(t *testing.T) {
	var output bytes.Buffer
	manager := New(Config{Output: &output, Columns: 120, Enabled: true})
	manager.AddFinalMessage("Images saved: 2/2")
	manager.Close(nil)

	if !strings.HasSuffix(output.String(), "Images saved: 2/2\n") {
		t.Fatalf("final message was not printed after progress: %q", output.String())
	}
}

func TestStatusFillerShowsSuccess(t *testing.T) {
	const width = 80

	var output bytes.Buffer
	if err := newStatusFiller(width).Fill(&output, decor.Statistics{
		AvailableWidth: width,
		Completed:      true,
	}); err != nil {
		t.Fatalf("newStatusFiller().Fill() error = %v", err)
	}

	line := strings.TrimSuffix(output.String(), "\n")
	if !strings.Contains(line, " ✓ ") || strings.ContainsAny(line, "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏") {
		t.Fatalf("success status filler = %q, want check mark without spinner", output.String())
	}
	if runewidth.StringWidth(line) != width {
		t.Fatalf("success status filler width = %d, want %d", runewidth.StringWidth(line), width)
	}
}

func TestStatusFillerShowsFailure(t *testing.T) {
	const width = 80

	var output bytes.Buffer
	if err := newStatusFiller(width).Fill(&output, decor.Statistics{
		AvailableWidth: width,
		Aborted:        true,
	}); err != nil {
		t.Fatalf("newStatusFiller().Fill() error = %v", err)
	}

	line := strings.TrimSuffix(output.String(), "\n")
	if !strings.Contains(line, " ✗ ") || strings.ContainsAny(line, "⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏") {
		t.Fatalf("failure status filler = %q, want cross without spinner", output.String())
	}
	if runewidth.StringWidth(line) != width {
		t.Fatalf("failure status filler width = %d, want %d", runewidth.StringWidth(line), width)
	}
}

func TestBarDecoratorsUseCompactHumanReadableColumns(t *testing.T) {
	name, nameWidth := decor.Name("DaemonSet", decor.WC{W: 12, C: decor.DindentRight}).Decor(decor.Statistics{})
	if name != "DaemonSet   " || nameWidth != 12 {
		t.Fatalf("name decorator = %q, width %d; want %q, width 12", name, nameWidth, "DaemonSet   ")
	}

	counters, countersWidth := decor.CountersNoUnit(
		"%d/%d ",
		decor.WC{W: fixedCounterWidth},
	).Decor(decor.Statistics{
		Total:   9,
		Current: 9,
	})
	wantCounters := strings.Repeat(" ", fixedCounterWidth-len("9/9 ")) + "9/9 "
	if counters != wantCounters || countersWidth != fixedCounterWidth {
		t.Fatalf("counter decorator = %q, width %d; want %q, width %d", counters, countersWidth, wantCounters, fixedCounterWidth)
	}

	percentage, percentageWidth := decor.NewPercentage("%d").Decor(decor.Statistics{
		Total:   9,
		Current: 9,
	})
	if percentage != "100%" || percentageWidth != 4 {
		t.Fatalf("percentage decorator = %q, width %d; want %q, width 4", percentage, percentageWidth, "100%")
	}

	percentage, percentageWidth = decor.NewPercentage(
		"%d",
		decor.WC{W: fixedPercentageWidth},
	).Decor(decor.Statistics{
		Total:   100,
		Current: 1,
	})
	wantPercentage := strings.Repeat(" ", fixedPercentageWidth-len("1%")) + "1%"
	if percentage != wantPercentage || percentageWidth != fixedPercentageWidth {
		t.Fatalf("aligned percentage decorator = %q, width %d; want %q, width %d",
			percentage, percentageWidth, wantPercentage, fixedPercentageWidth)
	}
}

func TestOverallLayoutMatchesDetailRows(t *testing.T) {
	base, ok := calculateLayout(160)
	if !ok {
		t.Fatal("calculateLayout() rejected a valid terminal width")
	}

	overall := calculateOverallLayout(base, "Images", calculateOverallCounterWidth(5))
	if overall.Columns != base.Columns {
		t.Fatalf("overall layout columns = %d, want %d", overall.Columns, base.Columns)
	}
}

func TestShouldEnableRejectsUnsafeOutput(t *testing.T) {
	if ShouldEnable(Policy{
		Stderr:          &bytes.Buffer{},
		TerminalColumns: 120,
	}) {
		t.Fatal("ShouldEnable() accepted a non-file writer")
	}
	if ShouldEnable(Policy{NoProgress: true}) {
		t.Fatal("ShouldEnable() ignored --no-progress")
	}
}

func TestCancellationDoesNotCloseProgressWriterBeforeManagerClose(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	manager := New(Config{
		Context: ctx,
		Output:  &bytes.Buffer{},
		Columns: 100,
		Enabled: true,
	})
	cancel()

	if _, err := manager.LogWriter().Write([]byte("after cancellation\n")); err != nil {
		t.Fatalf("progress log writer returned an error after context cancellation: %v", err)
	}

	manager.Close(context.Canceled)
}

func TestLateLogUsesOutputAfterManagerClose(t *testing.T) {
	var output bytes.Buffer
	manager := New(Config{Output: &output, Columns: 100, Enabled: true})
	writer := manager.LogWriter()
	manager.Close(context.Canceled)
	manager.Close(nil)

	if _, err := writer.Write([]byte("late cleanup\n")); err != nil {
		t.Fatalf("late log write = %v", err)
	}
	if !strings.Contains(output.String(), "late cleanup") {
		t.Fatalf("late log was not written to output: %q", output.String())
	}
}

func TestOperationCompletesDynamicBars(t *testing.T) {
	manager := New(Config{
		Context: context.Background(),
		Output:  &bytes.Buffer{},
		Columns: 100,
		Enabled: true,
	})
	operation := manager.NewOperation()
	operation.SetPlan(Plan{
		Scopes: []string{"default", "production"},
		Jobs: []Job{
			{Group: "ConfigMap", Scope: "default"},
			{Group: "Service", Scope: "default"},
			{Group: "Service", Scope: "production"},
		},
	})
	if operation.overall != nil || operation.scopeBar != nil || len(operation.groups) != 0 {
		t.Fatal("SetPlan() created progress bars before any objects were listed")
	}

	operation.AddTotal("ConfigMap", 1)
	if operation.overall == nil || operation.scopeBar == nil || len(operation.groups) != 1 {
		t.Fatal("AddTotal() did not create bars for a non-empty group")
	}
	operation.Increment("ConfigMap")
	operation.FinishJob(Job{Group: "ConfigMap", Scope: "default"})
	operation.AddTotal("Service", 2)
	if len(operation.groups) != 2 {
		t.Fatal("AddTotal() did not add the second non-empty group")
	}
	operation.Increment("Service")
	operation.Increment("Service")
	operation.FinishJob(Job{Group: "Service", Scope: "default"})
	operation.FinishJob(Job{Group: "Service", Scope: "production"})
	if !operation.overall.Completed() {
		t.Fatal("last finished job did not complete the aggregate bar")
	}
	operation.Close()
	manager.Close(nil)
}

func TestOperationAggregatesTotalsAcrossJobs(t *testing.T) {
	manager := New(Config{
		Context: context.Background(),
		Output:  &bytes.Buffer{},
		Columns: 100,
		Enabled: true,
	})
	operation := manager.NewOperation()
	operation.SetPlan(Plan{
		Scopes: []string{"default", "production"},
		Jobs: []Job{
			{Group: "ServiceAccount", Scope: "default"},
			{Group: "ServiceAccount", Scope: "production"},
		},
	})

	operation.AddTotal("ServiceAccount", 26)
	for range 26 {
		operation.Increment("ServiceAccount")
	}
	operation.FinishJob(Job{Group: "ServiceAccount", Scope: "default"})

	operation.AddTotal("ServiceAccount", 44)
	for range 44 {
		operation.Increment("ServiceAccount")
	}
	operation.FinishJob(Job{Group: "ServiceAccount", Scope: "production"})

	if !operation.groups["ServiceAccount"].Completed() {
		t.Fatal("aggregated detail bar did not complete")
	}
	if got := operation.groups["ServiceAccount"].Current(); got != 70 {
		t.Fatalf("aggregated detail bar current = %d, want 70", got)
	}

	operation.Close()
	manager.Close(nil)
}

func TestOperationSkipsEmptyGroups(t *testing.T) {
	manager := New(Config{
		Context: context.Background(),
		Output:  &bytes.Buffer{},
		Columns: 100,
		Enabled: true,
	})
	operation := manager.NewOperation()
	operation.SetPlan(Plan{
		Scopes: []string{"default"},
		Jobs:   []Job{{Group: "ConfigMap", Scope: "default"}},
	})

	operation.AddTotal("ConfigMap", 0)
	operation.FinishJob(Job{Group: "ConfigMap", Scope: "default"})

	if operation.overall != nil || operation.scopeBar != nil || len(operation.groups) != 0 {
		t.Fatal("empty operation created a 0/0 progress bar")
	}

	manager.Close(nil)
}

func TestOperationDoesNotCreateEmptyGroup(t *testing.T) {
	manager := New(Config{
		Context: context.Background(),
		Output:  &bytes.Buffer{},
		Columns: 100,
		Enabled: true,
	})
	operation := manager.NewOperation()
	operation.SetPlan(Plan{
		Scopes: []string{"default"},
		Jobs: []Job{
			{Group: "ConfigMap", Scope: "default"},
			{Group: "Service", Scope: "default"},
		},
	})

	operation.AddTotal("ConfigMap", 1)
	operation.Increment("ConfigMap")
	operation.FinishJob(Job{Group: "ConfigMap", Scope: "default"})
	operation.AddTotal("Service", 0)
	operation.FinishJob(Job{Group: "Service", Scope: "default"})

	if _, exists := operation.groups["ConfigMap"]; !exists {
		t.Fatal("non-empty planned group was removed from progress output")
	}
	if _, exists := operation.groups["Service"]; exists {
		t.Fatal("empty group entered progress output")
	}

	operation.Close()
	manager.Close(nil)
}

func TestBatchTracksLifecycleProgress(t *testing.T) {
	manager := New(Config{
		Context: context.Background(),
		Output:  &bytes.Buffer{},
		Columns: 100,
		Enabled: true,
	})
	batch := manager.NewBatch("PVCs", []string{"default/data"})
	batch.SetStage("default/data", "snapshot ready")
	batch.SetProgress("default/data", 35)
	batch.SetProgress("default/data", 20)

	bar := batch.items["default/data"]
	if got := bar.Current(); got != 35 {
		t.Fatalf("batch item progress = %d, want 35", got)
	}
	batch.Complete("default/data")
	if got := bar.Current(); got != 100 {
		t.Fatalf("completed batch item progress = %d, want 100", got)
	}

	batch.Close(nil)
	manager.Close(nil)
}

func TestBatchEnabledMatchesVisibleBars(t *testing.T) {
	manager := New(Config{
		Context: context.Background(),
		Output:  &bytes.Buffer{},
		Columns: 100,
		Enabled: true,
	})

	batch := manager.NewBatch("PVCs", []string{"default/data"})
	if !batch.Enabled() {
		t.Fatal("enabled batch reported no visible bars")
	}
	batch.Close(nil)
	if manager.NewBatch("PVCs", nil).Enabled() {
		t.Fatal("empty batch reported visible bars")
	}
	manager.Close(nil)
}
