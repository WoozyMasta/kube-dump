// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package kube

import (
	"context"
	"errors"
	"fmt"
	"path"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/rs/zerolog/log"
	fieldcrypto "github.com/woozymasta/kube-dump/v2/internal/crypto/fields"
	"github.com/woozymasta/kube-dump/v2/internal/profile"
	"github.com/woozymasta/kube-dump/v2/internal/state"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
)

const defaultListPageSize int64 = 500

const maxCollectionWarningDetails = 8

// StateSink receives normalized objects during collection.
type StateSink interface {
	// Write persists one normalized object
	// and reports whether its content differs from an existing object
	// at the same canonical identity.
	Write(ctx context.Context, object state.Object) (changed bool, err error)
}

// Selection controls which discovered API resources and objects are collected.
type Selection struct {
	LabelSelector             string   `long:"label-selector"              description:"Include objects matching Kubernetes label selector syntax" short:"L"`
	AnnotationSelector        string   `long:"annotation-selector"         description:"Include objects matching Kubernetes annotation selector syntax"`
	ExcludeAnnotationSelector string   `long:"exclude-annotation-selector" description:"Exclude objects matching this annotation selector"`
	Resources                 []string `long:"resource"                    description:"Include resources by plural name, full GVR, or kubectl short name" short:"r"`
	Namespaces                []string `long:"namespace"                   description:"Include only these namespaces" short:"n"`
	ExcludeNamespaces         []string `long:"exclude-namespace"           description:"Exclude these namespaces from namespaced collection"`
}

// CollectionOptions controls how the collector executes discovery and list jobs.
// Strict changes non-fatal discovery and collection warnings into errors;
// Concurrency and PageSize bound parallelism and individual list requests.
type CollectionOptions struct {
	Strict      bool  `long:"strict"      description:"Abort on discovery, permission, or collection errors"`
	Concurrency int   `long:"concurrency" description:"Maximum parallel Kubernetes resource requests"           default:"4"`
	PageSize    int64 `long:"page-size"   description:"Maximum objects returned by one Kubernetes list request" default:"500"`
}

// CollectionObserver receives best-effort events from one collection run.
// Implementations must be safe for concurrent calls and must not return errors;
// observation is presentation-only and never changes collection semantics.
type CollectionObserver interface {
	// Plan reports the complete job set before collection workers start.
	Plan(CollectionPlan)
	// List reports the number of objects returned for one resource job.
	List(CollectionListEvent)
	// Object reports one listed object after selection and sink processing.
	Object(CollectionObjectEvent)
	// JobFinished reports that one resource job has stopped, with its error if any.
	JobFinished(CollectionJobEvent)
}

// CollectionPlan describes the jobs and scopes known before worker execution.
type CollectionPlan struct {
	// Jobs contains one entry for every resource and namespace combination.
	Jobs []CollectionJobInfo
	// Scopes contains namespaces or the cluster scope used by the jobs.
	Scopes []string
}

// CollectionJobInfo identifies one resource listing operation.
type CollectionJobInfo struct {
	// Group is the Kubernetes API group, empty for core resources.
	Group string
	// Version is the served Kubernetes API version.
	Version string
	// Resource is the plural API resource name.
	Resource string
	// Kind is the Kubernetes kind used for display grouping.
	Kind string
	// Namespace is empty for a cluster-scoped job.
	Namespace string
	// Namespaced reports whether the job addresses one namespace.
	Namespaced bool
}

// CollectionListEvent reports the result of one completed list operation.
type CollectionListEvent struct {
	// Job identifies the listed resource and scope.
	Job CollectionJobInfo
	// Objects is the number of objects returned by the list operation.
	Objects int
}

// CollectionObjectEvent reports the outcome for one listed object.
type CollectionObjectEvent struct {
	// Job identifies the resource and scope that produced the object.
	Job CollectionJobInfo
	// Selected reports that resource and metadata selectors accepted the object.
	Selected bool
	// Skipped reports that the object was rejected by selection rules.
	Skipped bool
	// Collected reports that the accepted object was written successfully.
	Collected bool
	// Changed reports that the sink replaced different existing content.
	Changed bool
}

// CollectionJobEvent reports completion of one collection job.
type CollectionJobEvent struct {
	// Err contains the job failure, if one occurred.
	Err error
	// Job identifies the completed resource and scope.
	Job CollectionJobInfo
}

// Collector discovers resources, normalizes objects, and writes them to a canonical state sink.
type Collector struct {
	// compiled contains selectors reused by workers.
	compiled compiledSelection
	// Dynamic lists and reads Kubernetes objects.
	Dynamic dynamic.Interface
	// Discovery provides API resource and namespace metadata.
	Discovery discovery.DiscoveryInterface
	// Sink persists normalized objects and change information.
	Sink StateSink
	// Observer receives optional progress events and is never required for collection.
	Observer CollectionObserver
	// Profile controls canonical object normalization.
	Profile *profile.CompiledProfile
	// namespaceResource is retained from discovery even when resource selection excludes Namespaces.
	namespaceResource *discoveredResource
	// Selection controls resource and metadata filtering.
	Selection Selection
	// FieldEncryption controls encryption of profile-selected resource fields.
	FieldEncryption fieldcrypto.Options
	// Collection controls execution behavior for discovery and list jobs.
	Collection CollectionOptions
}

// CollectionResult summarizes a collection without relying on worker order.
type CollectionResult struct {
	// Warnings contains up to maxCollectionWarningDetails non-fatal errors from non-strict collection.
	Warnings     []error
	Identities   []state.Identity // Identities contains objects successfully written by this run.
	WarningCount int              // WarningCount is the exact total.
	Listed       int              // Listed is the number of objects returned by Kubernetes list calls.
	Selected     int              // Selected is the number of listed objects passing metadata filters.
	Skipped      int              // Skipped is the number of listed objects rejected by selection filters.
	Collected    int              // Collected is the number of selected objects successfully written.
	Changed      int              // Changed is the number of writes that changed existing content.
	Failed       int              // Failed is the number of resource jobs that returned an error.
}

// WarningTotal returns the exact number of non-fatal collection warnings.
func (r CollectionResult) WarningTotal() int {
	if r.WarningCount < len(r.Warnings) {
		return len(r.Warnings)
	}

	return r.WarningCount
}

// OmittedWarningDetails returns the number of warnings not retained in Warnings.
func (r CollectionResult) OmittedWarningDetails() int {
	return max(0, r.WarningTotal()-len(r.Warnings))
}

// addWarning records a warning while retaining only a bounded set of details.
func (r *CollectionResult) addWarning(err error) {
	if err == nil {
		return
	}

	r.WarningCount++
	if len(r.Warnings) < maxCollectionWarningDetails {
		r.Warnings = append(r.Warnings, err)
	}
}

// collectionStats contains per-job counters before they are merged by workers.
type collectionStats struct {
	// identities contains objects successfully written by this job.
	identities []state.Identity
	// listed is the number of objects returned by list calls.
	listed int
	// selected is the number passing metadata and profile filters.
	selected int
	// skipped is the number rejected by filters.
	skipped int
	// collected is the number successfully written.
	collected int
	// changed is the number of writes replacing different content.
	changed int
}

// identitySet detects duplicate identities during one concurrent collection.
// It is deliberately not serializable: the current backup format has no
// global index or persisted collection state.
type identitySet struct {
	// values contains identities already accepted in this run.
	values map[string]struct{}
	// mutex protects values during concurrent collection.
	mutex sync.Mutex
}

// discoveredResource is the minimum discovery metadata required by a job.
type discoveredResource struct {
	// Group is the API group, empty for core resources.
	Group string
	// Version is the served API version.
	Version string
	// API contains discovery metadata for the resource.
	API metav1.APIResource
}

// collectionJob identifies one resource list operation.
type collectionJob struct {
	// Resource is the discovery entry to list.
	Resource discoveredResource
	// Namespace is the target namespace, or empty for cluster scope.
	Namespace string
}

// compiledSelection contains parsed selectors reused by every collection worker.
type compiledSelection struct {
	// labelInclude contains the positive label selector.
	labelInclude labels.Selector
	// annotationInclude contains the positive annotation selector.
	annotationInclude labels.Selector
	// annotationExclude contains the negative annotation selector.
	annotationExclude labels.Selector
}

// newIdentitySet creates an empty per-run identity set.
func newIdentitySet() *identitySet {
	return &identitySet{values: make(map[string]struct{})}
}

// Add validates and records one identity, rejecting duplicates in one run.
func (s *identitySet) Add(identity state.Identity) error {
	if s == nil {
		return errors.New("identity set is nil")
	}
	if err := identity.Validate(); err != nil {
		return err
	}

	s.mutex.Lock()
	defer s.mutex.Unlock()

	if _, exists := s.values[identity.Key()]; exists {
		return fmt.Errorf("duplicate collected identity %q", identity.Key())
	}
	s.values[identity.Key()] = struct{}{}

	return nil
}

// compileSelection parses all static selectors once before collection starts.
func compileSelection(selection Selection) (compiledSelection, error) {
	parse := func(name, value string) (labels.Selector, error) {
		if value == "" {
			return nil, nil
		}

		parsed, err := labels.Parse(value)
		if err != nil {
			return nil, fmt.Errorf("parse %s: %w", name, err)
		}

		return parsed, nil
	}

	labelInclude, err := parse("label selector", selection.LabelSelector)
	if err != nil {
		return compiledSelection{}, err
	}

	annotationInclude, err := parse("annotation selector", selection.AnnotationSelector)
	if err != nil {
		return compiledSelection{}, err
	}

	annotationExclude, err := parse("exclude annotation selector", selection.ExcludeAnnotationSelector)
	if err != nil {
		return compiledSelection{}, err
	}

	return compiledSelection{
		labelInclude:      labelInclude,
		annotationInclude: annotationInclude,
		annotationExclude: annotationExclude,
	}, nil
}

// allowsSet applies include and inverse selectors to one metadata set.
func (s compiledSelection) allowsSet(values labels.Set, include, exclude labels.Selector) bool {
	if include != nil && !include.Matches(values) {
		return false
	}

	return exclude == nil || !exclude.Matches(values)
}

// AllowsIdentity reports whether a canonical object belongs to this selection.
// It mirrors the resource and namespace filters used during discovery.
// The core Namespace resource remains eligible during discovery because it is
// required to resolve namespaced jobs, even when the profile omits it.
func (s Selection) AllowsIdentity(identity state.Identity) bool {
	if identity.Namespace == "" && len(s.Namespaces) > 0 {
		return false
	}
	if identity.Namespace != "" && !s.NamespaceSelected(identity.Namespace) {
		return false
	}

	if !resourceSelectionMatches(s.Resources, func(expression profile.ResourceExpression) bool {
		return expression.Matches(
			identity.Group,
			identity.Version,
			identity.Resource,
			identity.Name,
		)
	}) {
		return false
	}

	return true
}

// AllowsObject reports whether an object passes resource and metadata filters.
// Metadata selectors are evaluated after identity and namespace rules,
// matching the collector's filtering order before normalization.
func (s Selection) AllowsObject(object state.Object) bool {
	return s.AllowsIdentity(object.Identity) &&
		s.allowsUnstructured(*object.Value)
}

// Validate checks selector and namespace settings before discovery starts.
// Empty selectors are neutral;
// non-empty values must be valid Kubernetes label selectors and must not contain line breaks.
func (s Selection) Validate() error {
	selectors := []struct {
		name  string
		value string
	}{
		{name: "label selector", value: s.LabelSelector},
		{name: "annotation selector", value: s.AnnotationSelector},
		{name: "exclude annotation selector", value: s.ExcludeAnnotationSelector},
	}

	for _, selector := range selectors {
		name, value := selector.name, selector.value
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%s contains a newline", name)
		}

		if _, err := labels.Parse(value); err != nil {
			return fmt.Errorf("parse %s: %w", name, err)
		}
	}

	for index, namespace := range s.Namespaces {
		pattern := strings.TrimPrefix(namespace, "!")
		if pattern == "" {
			return fmt.Errorf("namespace pattern[%d] is invalid", index)
		}
		if _, err := path.Match(pattern, ""); err != nil {
			return fmt.Errorf("invalid namespace pattern %q: %w", namespace, err)
		}
	}

	for _, resource := range s.Resources {
		if _, err := profile.ParseResourceExpression(resource); err != nil {
			return fmt.Errorf("parse resource expression %q: %w", resource, err)
		}
	}

	return nil
}

// patternSelectionMatches evaluates ordered string patterns.
// A positive expression starts an include list; when no positive expressions exist,
// selection starts unrestricted and `!` entries act as exclusions.
// Every later matching expression replaces the previous decision.
func patternSelectionMatches(values []string, matches func(string) bool) bool {
	hasInclude := false
	for _, value := range values {
		if !strings.HasPrefix(value, "!") {
			hasInclude = true
			break
		}
	}

	selected := !hasInclude
	for _, value := range values {
		exclude := strings.HasPrefix(value, "!")
		pattern := strings.TrimPrefix(value, "!")
		if matches(pattern) {
			selected = !exclude
		}
	}

	return selected
}

// resourceSelectionMatches evaluates ordered resource expressions.
// A positive expression starts an include list; when no positive expressions exist,
// selection starts unrestricted and `!` entries act as exclusions.
func resourceSelectionMatches(
	values []string,
	matches func(profile.ResourceExpression) bool,
) bool {
	hasInclude := false
	for _, value := range values {
		expression, err := profile.ParseResourceExpression(value)
		if err == nil && !expression.Exclude {
			hasInclude = true
			break
		}
	}

	selected := !hasInclude
	for _, value := range values {
		expression, err := profile.ParseResourceExpression(value)
		if err == nil && matches(expression) {
			selected = !expression.Exclude
		}
	}

	return selected
}

// resourceDiscoverySelectionMatches applies ordered expressions to discovery entries.
// A name-specific exclusion is ignored so the parent resource can still be listed
// and the exclusion can be applied to returned objects.
func resourceDiscoverySelectionMatches(
	values []string,
	matches func(profile.ResourceExpression) bool,
) bool {
	hasInclude := false
	for _, value := range values {
		expression, err := profile.ParseResourceExpression(value)
		if err == nil && !expression.Exclude {
			hasInclude = true
			break
		}
	}

	selected := !hasInclude
	for _, value := range values {
		expression, err := profile.ParseResourceExpression(value)
		if err != nil || (expression.Exclude && expression.Name != "") {
			continue
		}
		if matches(expression) {
			selected = !expression.Exclude
		}
	}

	return selected
}

// allowsSet applies include and inverse selectors to metadata.
func (s Selection) allowsSet(values labels.Set, include, exclude string) bool {
	if include != "" {
		includeSelector, _ := labels.Parse(include)
		if !includeSelector.Matches(values) {
			return false
		}
	}

	if exclude == "" {
		return true
	}

	excludeSelector, _ := labels.Parse(exclude)
	return !excludeSelector.Matches(values)
}

// Allows reports whether a discovered resource is selected.
// Discovery entries without a listable resource name
// or verb are rejected before jobs are built.
func (s Selection) Allows(resource metav1.APIResource, group, version string) bool {
	if len(resource.Name) == 0 || strings.Contains(resource.Name, "/") {
		return false
	}

	if !resourceDiscoverySelectionMatches(s.Resources, func(expression profile.ResourceExpression) bool {
		return expression.MatchesResource(group, version, resource.Name)
	}) {
		return false
	}

	return containsVerb(resource.Verbs, "list")
}

// NamespaceSelected reports whether a namespace is allowed by the selection.
func (s Selection) NamespaceSelected(namespace string) bool {
	if namespace == "" || contains(s.ExcludeNamespaces, namespace) {
		return false
	}

	return len(s.Namespaces) == 0 || patternSelectionMatches(
		s.Namespaces,
		func(pattern string) bool {
			matched, err := path.Match(pattern, namespace)
			return err == nil && matched
		},
	)
}

// Collect discovers listable resources and writes their normalized objects.
func (c *Collector) Collect(ctx context.Context) (CollectionResult, error) {
	if ctx == nil {
		return CollectionResult{}, errors.New("collector context is required")
	}

	// Validate dependencies before making any API request
	// so configuration errors fail fast and do not produce a partial backup directory.
	if err := c.validate(); err != nil {
		return CollectionResult{}, err
	}

	log.Logger.Debug().
		Str("component", "collector").
		Int("concurrency", c.Collection.Concurrency).
		Int64("page_size", c.Collection.PageSize).
		Msg("collector configuration validated")

	resources, discoveryErr := c.discoverResources()

	log.Logger.Debug().
		Str("component", "collector").
		Int("resources", len(resources)).
		Bool("discovery_warning", discoveryErr != nil).
		Msg("Kubernetes resources discovered")

	// Discovery may return usable partial results together with a warning.
	// In strict mode that warning is promoted to a fatal collection error.
	if discoveryErr != nil && c.Collection.Strict {
		return CollectionResult{}, discoveryErr
	}

	result := CollectionResult{}
	if discoveryErr != nil {
		result.addWarning(discoveryErr)
	}

	namespaces, err := c.namespaces(ctx, resources)
	if err != nil {
		// Without an explicit namespace list,
		// inability to list namespaces means namespaced resources cannot be addressed safely.
		if c.Collection.Strict {
			return result, err
		}
		result.addWarning(err)
	}

	log.Logger.Debug().
		Str("component", "collector").
		Int("namespaces", len(namespaces)).
		Msg("Kubernetes namespaces resolved")

	jobs := buildJobs(resources, namespaces, len(c.Selection.Namespaces) > 0)
	c.observePlan(jobs)
	if len(jobs) == 0 {
		planErr := errors.New("resource selection matched no listable Kubernetes resources or namespaces")
		if len(result.Warnings) > 0 {
			return result, errors.Join(planErr, errors.Join(result.Warnings...))
		}

		return result, planErr
	}

	log.Logger.Debug().
		Str("component", "collector").
		Int("jobs", len(jobs)).
		Msg("Kubernetes collection jobs created")

	return c.runJobs(ctx, jobs, result)
}

// validate checks required dependencies and applies safe worker defaults.
func (c *Collector) validate() error {
	if c.Dynamic == nil {
		return errors.New("collector dynamic client is required")
	}
	if c.Discovery == nil {
		return errors.New("collector discovery client is required")
	}
	if c.Sink == nil {
		return errors.New("collector state sink is required")
	}

	if err := c.Selection.Validate(); err != nil {
		return err
	}

	compiled, err := compileSelection(c.Selection)
	if err != nil {
		return err
	}
	c.compiled = compiled
	if c.Profile == nil {
		return errors.New("collector profile is required")
	}

	// Keep zero-value collectors usable for callers that do not configure
	// field encryption explicitly.
	if c.FieldEncryption.Mode == "" {
		c.FieldEncryption.Mode = fieldcrypto.Plain
	}

	if err := c.FieldEncryption.Mode.Validate(); err != nil {
		return err
	}

	if c.FieldEncryption.Mode == fieldcrypto.Age && len(c.FieldEncryption.Recipients) == 0 {
		return errors.New("collector age field encryption requires at least one recipient")
	}
	if c.FieldEncryption.Mode == fieldcrypto.AES256SIV &&
		(c.FieldEncryption.SIVCipher == nil || c.FieldEncryption.SIVKeyID == "") {
		return errors.New("collector AES-SIV field encryption requires a keyring")
	}
	if c.Collection.Concurrency < 0 {
		return errors.New("collector concurrency must not be negative")
	}
	if c.Collection.PageSize < 0 {
		return errors.New("collector page size must not be negative")
	}

	return nil
}

// discoverResources flattens and sorts preferred discovery results.
func (c *Collector) discoverResources() ([]discoveredResource, error) {
	c.namespaceResource = nil
	lists, err := c.Discovery.ServerPreferredResources()
	selection, aliasErr := resolveResourceAliases(c.Selection, lists)
	if aliasErr != nil {
		return nil, aliasErr
	}

	c.Selection = selection
	resources := make([]discoveredResource, 0)

	for _, list := range lists {
		groupVersion, parseErr := schema.ParseGroupVersion(list.GroupVersion)
		if parseErr != nil {
			// A malformed discovery entry cannot be mapped to a GVR.
			// Non-strict collection skips only that entry and retains other API resources.
			if c.Collection.Strict {
				return nil, fmt.Errorf("parse discovered groupVersion %q: %w", list.GroupVersion, parseErr)
			}

			continue
		}

		for _, resource := range list.APIResources {
			if groupVersion.Group == "" && groupVersion.Version == "v1" && resource.Name == "namespaces" &&
				containsVerb(resource.Verbs, "list") {
				candidate := discoveredResource{
					Group: groupVersion.Group, Version: groupVersion.Version,
					API: resource,
				}
				c.namespaceResource = &candidate
			}

			if !c.Selection.Allows(resource, groupVersion.Group, groupVersion.Version) {
				continue
			}

			resources = append(resources, discoveredResource{
				Group: groupVersion.Group, Version: groupVersion.Version,
				API: resource,
			})
		}
	}

	sort.Slice(resources, func(left, right int) bool {
		return resourceKey(resources[left]) < resourceKey(resources[right])
	})

	return resources, discoveryError(err)
}

// discoveryError preserves partial discovery information
// while making the failure visible to the caller's strict/non-strict policy.
func discoveryError(err error) error {
	if err == nil {
		return nil
	}

	return fmt.Errorf("discover Kubernetes resources: %w", err)
}

// namespaces resolves selected namespaces from discovery or the API.
func (c *Collector) namespaces(ctx context.Context, resources []discoveredResource) ([]string, error) {
	if len(c.Selection.Namespaces) > 0 && exactNamespacePatterns(c.Selection.Namespaces) {
		// Explicit namespaces avoid an additional API request
		// and are sorted to make job creation independent of CLI argument order.
		namespaces := make([]string, 0, len(c.Selection.Namespaces))
		for _, namespace := range c.Selection.Namespaces {
			if c.Selection.NamespaceSelected(namespace) {
				namespaces = append(namespaces, namespace)
			}
		}

		sort.Strings(namespaces)
		return namespaces, nil
	}

	resource, found := c.namespaceResource, c.namespaceResource != nil
	if !found {
		for index := range resources {
			candidate := &resources[index]
			if candidate.Group == "" && candidate.Version == "v1" && candidate.API.Name == "namespaces" {
				resource = candidate
				found = true
				break
			}
		}
	}

	if found {
		// Namespace discovery uses the same paginated list path as ordinary resources,
		// preserving one collection behavior.
		// Namespace discovery must not inherit the object label selector:
		// a selector intended for ConfigMaps or Deployments could otherwise
		// hide every Namespace needed to build namespaced collection jobs.
		objects := make([]unstructured.Unstructured, 0)
		_, err := c.listEach(ctx, *resource, "", "", "", func(object unstructured.Unstructured) error {
			objects = append(objects, object)
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("list namespaces: %w", err)
		}

		namespaces := make([]string, 0, len(objects))
		for _, object := range objects {
			name := object.GetName()
			if c.Selection.NamespaceSelected(name) {
				namespaces = append(namespaces, name)
			}
		}

		sort.Strings(namespaces)
		return namespaces, nil
	}

	return nil, errors.New("discovery did not expose core/v1 namespaces")
}

// buildJobs creates deterministic work units for cluster and namespaced APIs.
func buildJobs(resources []discoveredResource, namespaces []string, namespacedOnly bool) []collectionJob {
	jobs := make([]collectionJob, 0)
	for _, resource := range resources {
		if resource.API.Namespaced {
			for _, namespace := range namespaces {
				jobs = append(jobs, collectionJob{Resource: resource, Namespace: namespace})
			}
			continue
		}

		if namespacedOnly {
			continue
		}

		jobs = append(jobs, collectionJob{Resource: resource})
	}

	return jobs
}

// runJobs executes collection jobs with a bounded worker pool.
func (c *Collector) runJobs(ctx context.Context, jobs []collectionJob, result CollectionResult) (CollectionResult, error) {
	workers := c.Collection.Concurrency

	// Zero means use the bounded default;
	// negative values were rejected by validate before this method was called.
	if workers == 0 {
		workers = 4
	}
	if workers > len(jobs) && len(jobs) > 0 {
		workers = len(jobs)
	}

	if workers == 0 {
		log.Logger.Debug().
			Str("component", "collector").
			Int("jobs", 0).
			Int("workers", 0).
			Msg("no Kubernetes collection jobs to run")
		return result, nil
	}

	log.Logger.Debug().
		Str("component", "collector").
		Int("jobs", len(jobs)).
		Int("workers", workers).
		Msg("Kubernetes collection workers started")

	workerContext, cancel := context.WithCancel(ctx)
	defer cancel()

	identities := newIdentitySet()
	jobChannel := make(chan collectionJob)
	var waitGroup sync.WaitGroup
	var mutex sync.Mutex
	var firstErr error

	worker := func() {
		defer waitGroup.Done()
		for {
			var job collectionJob
			var open bool

			select {
			case job, open = <-jobChannel:
				if !open {
					return
				}
			case <-workerContext.Done():
				return
			}

			// Collection is parallel, but aggregate counters and warning ownership
			// are serialized below to keep the result consistent.
			stats, err := c.collectJob(workerContext, job, identities)
			c.observeJobFinished(job, err)
			mutex.Lock()

			// Counters, warnings, and the strict-mode error
			// are shared across workers and must be updated under one mutex.
			result.Listed += stats.listed
			result.Selected += stats.selected
			result.Skipped += stats.skipped
			result.Collected += stats.collected
			result.Changed += stats.changed
			result.Identities = append(result.Identities, stats.identities...)

			if err != nil {
				result.Failed++
				if c.Collection.Strict && firstErr == nil {
					firstErr = err
					cancel()
				} else if !c.Collection.Strict {
					result.addWarning(err)
				}
			}

			mutex.Unlock()
		}
	}

	waitGroup.Add(workers)
	for range workers {
		go worker()
	}

	// Stop feeding jobs after a strict-mode failure.
	// Workers that already hold a job observe cancellation
	// and exit through the same cleanup path.
	for _, job := range jobs {
		select {
		case jobChannel <- job:

		case <-workerContext.Done():
			// Closing the channel lets already-started workers finish cleanly
			// before the cancellation error is returned to the caller.
			close(jobChannel)
			waitGroup.Wait()
			if firstErr != nil {
				return result, firstErr
			}

			return result, fmt.Errorf("collect resources: %w", workerContext.Err())
		}
	}

	close(jobChannel)
	waitGroup.Wait()

	if firstErr != nil {
		return result, firstErr
	}

	log.Logger.Debug().
		Str("component", "collector").
		Int("jobs", len(jobs)).
		Int("collected", result.Collected).
		Int("changed", result.Changed).
		Int("failed", result.Failed).
		Msg("Kubernetes collection workers finished")

	return result, nil
}

// collectJob lists one resource job and writes every normalized object.
func (c *Collector) collectJob(
	ctx context.Context,
	job collectionJob,
	identities *identitySet,
) (collectionStats, error) {
	log.Logger.Trace().
		Str("component", "collector").
		Str("operation", "list").
		Str("resource", resourceKey(job.Resource)).
		Str("namespace", job.Namespace).
		Msg("collecting Kubernetes resource")

	var stats collectionStats
	listed, err := c.listEach(
		ctx,
		job.Resource,
		job.Namespace,
		c.Selection.LabelSelector,
		c.Selection.ObjectNameFieldSelector(
			job.Resource.Group,
			job.Resource.Version,
			job.Resource.API.Name,
		),
		func(value unstructured.Unstructured) error {
			// Identity is derived from discovery plus API metadata;
			// serialized YAML is deliberately not used as the canonical identity source.
			identity := state.Identity{
				Group:     job.Resource.Group,
				Version:   job.Resource.Version,
				Resource:  job.Resource.API.Name,
				Kind:      job.Resource.API.Kind,
				Namespace: value.GetNamespace(),
				Name:      value.GetName(),
			}

			if !c.Selection.AllowsIdentity(identity) {
				stats.skipped++
				c.observeObject(job, CollectionObjectEvent{Skipped: true})
				return nil
			}

			if !c.Profile.AllowsResourceName(identity.Name) {
				stats.skipped++
				c.observeObject(job, CollectionObjectEvent{Skipped: true})
				return nil
			}

			if !c.compiled.allowsSet(
				labels.Set(value.GetLabels()),
				c.compiled.labelInclude,
				nil,
			) || !c.compiled.allowsSet(
				labels.Set(value.GetAnnotations()),
				c.compiled.annotationInclude,
				c.compiled.annotationExclude,
			) {
				stats.skipped++
				c.observeObject(job, CollectionObjectEvent{Skipped: true})
				return nil
			}

			object, err := state.NewObject(identity, &value)
			if err != nil {
				return fmt.Errorf("construct %s/%s: %w", resourceKey(job.Resource), value.GetName(), err)
			}

			stats.selected++

			// Apply normalization and field encryption before writing to the sink;
			// the state package creates a separate object value for these mutations.
			normalized, err := c.Profile.Apply(object)
			if err != nil {
				return fmt.Errorf("apply profile to %s/%s: %w", resourceKey(job.Resource), value.GetName(), err)
			}

			encryptionPaths := c.Profile.EncryptionPaths(object)
			if len(encryptionPaths) > 0 {
				// Encryption is driven exclusively by profile paths.
				// The same walker handles fields in built-in resources and custom resources.
				normalized, err = fieldcrypto.EncryptFields(normalized, encryptionPaths, c.FieldEncryption)
				if err != nil {
					return fmt.Errorf("protect %s/%s: %w", resourceKey(job.Resource), value.GetName(), err)
				}
			}

			wasChanged, err := c.Sink.Write(ctx, normalized)
			if err != nil {
				return fmt.Errorf("write %s/%s: %w", resourceKey(job.Resource), value.GetName(), err)
			}

			// The store reports content changes independently from collection order,
			// which keeps repeated backups Git-friendly.
			if wasChanged {
				stats.changed++
			}

			if err := identities.Add(identity); err != nil {
				return fmt.Errorf("record %s/%s: %w", resourceKey(job.Resource), value.GetName(), err)
			}

			stats.identities = append(stats.identities, identity)
			stats.collected++
			c.observeObject(job, CollectionObjectEvent{
				Selected:  true,
				Collected: true,
				Changed:   wasChanged,
			})

			return nil
		},
	)
	if err != nil {
		// Forbidden list operations are expected with partial RBAC.
		// The caller decides whether this warning is fatal based on strict mode.
		if apierrors.IsForbidden(err) {
			return stats, fmt.Errorf("list %s %s: forbidden: %w", resourceKey(job.Resource), job.Namespace, err)
		}

		return stats, fmt.Errorf("list %s %s: %w", resourceKey(job.Resource), job.Namespace, err)
	}

	stats.listed = listed
	log.Logger.Trace().
		Str("component", "collector").
		Str("operation", "list").
		Str("resource", resourceKey(job.Resource)).
		Str("namespace", job.Namespace).Int("objects", listed).
		Msg("Kubernetes resource listed")
	c.observeList(job, stats.listed)

	return stats, nil
}

// observePlan publishes the immutable job plan before workers begin.
func (c *Collector) observePlan(jobs []collectionJob) {
	if c.Observer == nil {
		return
	}

	plan := CollectionPlan{
		Jobs:   make([]CollectionJobInfo, 0, len(jobs)),
		Scopes: make([]string, 0, len(jobs)),
	}
	seenScopes := make(map[string]struct{})

	for _, job := range jobs {
		info := collectionJobInfo(job)
		plan.Jobs = append(plan.Jobs, info)

		scope := info.Namespace
		if scope == "" {
			scope = "cluster"
		}
		if _, exists := seenScopes[scope]; !exists {
			seenScopes[scope] = struct{}{}
			plan.Scopes = append(plan.Scopes, scope)
		}
	}

	sort.Strings(plan.Scopes)
	c.Observer.Plan(plan)
}

// observeList publishes the total returned by one successful list operation.
func (c *Collector) observeList(job collectionJob, objects int) {
	if c.Observer == nil {
		return
	}

	c.Observer.List(CollectionListEvent{
		Job:     collectionJobInfo(job),
		Objects: objects,
	})
}

// observeObject publishes one object outcome after selection and persistence.
func (c *Collector) observeObject(job collectionJob, event CollectionObjectEvent) {
	if c.Observer == nil {
		return
	}

	event.Job = collectionJobInfo(job)
	c.Observer.Object(event)
}

// observeJobFinished publishes a job terminal event, including non-fatal errors.
func (c *Collector) observeJobFinished(job collectionJob, err error) {
	if c.Observer == nil {
		return
	}

	c.Observer.JobFinished(CollectionJobEvent{
		Job: collectionJobInfo(job),
		Err: err,
	})
}

// collectionJobInfo converts internal discovery state into observer data.
func collectionJobInfo(job collectionJob) CollectionJobInfo {
	return CollectionJobInfo{
		Group:      job.Resource.Group,
		Version:    job.Resource.Version,
		Resource:   job.Resource.API.Name,
		Kind:       job.Resource.API.Kind,
		Namespace:  job.Namespace,
		Namespaced: job.Resource.API.Namespaced,
	}
}

// allowsUnstructured applies metadata selectors before normalization and writes.
func (s Selection) allowsUnstructured(value unstructured.Unstructured) bool {
	return s.allowsSet(
		labels.Set(value.GetLabels()),
		s.LabelSelector,
		"",
	) && s.allowsSet(
		labels.Set(value.GetAnnotations()),
		s.AnnotationSelector,
		s.ExcludeAnnotationSelector,
	)
}

// listEach reads all pages for one resource job
// and processes each object before requesting the next page.
// This keeps memory bounded by one page
// instead of retaining the complete resource list for every worker job.
func (c *Collector) listEach(
	ctx context.Context,
	resource discoveredResource,
	namespace, labelSelector string,
	fieldSelector string,
	process func(unstructured.Unstructured) error,
) (int, error) {
	if process == nil {
		return 0, errors.New("kubernetes list callback is required")
	}

	pageSize := c.Collection.PageSize

	// A bounded page size prevents a single list request
	// from loading an unbounded resource set into memory.
	if pageSize == 0 {
		pageSize = defaultListPageSize
	}

	resourceNamespaceable := c.Dynamic.Resource(schema.GroupVersionResource{
		Group:    resource.Group,
		Version:  resource.Version,
		Resource: resource.API.Name,
	})

	var resourceClient dynamic.ResourceInterface = resourceNamespaceable
	if resource.API.Namespaced {
		resourceClient = resourceNamespaceable.Namespace(namespace)
	}

	continueToken := ""
	page := 0
	listed := 0

	for {
		page++
		list, err := resourceClient.List(ctx, metav1.ListOptions{
			Continue:      continueToken,
			Limit:         pageSize,
			LabelSelector: labelSelector,
			FieldSelector: fieldSelector,
		})
		if err != nil {
			return listed, err
		}

		listed += len(list.Items)

		log.Logger.Trace().
			Str("component", "collector").
			Str("operation", "list").
			Str("resource", resourceKey(resource)).
			Str("namespace", namespace).
			Int("page", page).
			Int("objects", len(list.Items)).
			Msg("Kubernetes resource page received")

		for index := range list.Items {
			if err := process(list.Items[index]); err != nil {
				return listed, err
			}
		}

		continueToken = list.GetContinue()

		// An empty continue token is the API contract for the final page.
		if continueToken == "" {
			return listed, nil
		}
	}
}

// resourceKey returns a stable human-readable discovery key.
func resourceKey(resource discoveredResource) string {
	if resource.Group == "" {
		return resource.Version + "/" + resource.API.Name
	}

	return resource.Group + "/" + resource.Version + "/" + resource.API.Name
}

// containsVerb checks discovery verbs without assuming their order.
func containsVerb(verbs []string, wanted string) bool {
	return slices.Contains(verbs, wanted)
}

// contains reports whether values contains value.
func contains(values []string, value string) bool {
	return slices.Contains(values, value)
}
