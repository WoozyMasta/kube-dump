package profile

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/state"
	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"oras.land/oras-go/v2/registry"
)

func TestLoadBuiltInProfiles(t *testing.T) {
	for _, name := range []string{"backup", "export", "raw"} {
		compiled, err := Load(name)
		if err != nil {
			t.Fatalf("Load(%q) error = %v", name, err)
		}

		if compiled.Name() != name {
			t.Errorf("Load(%q).Name() = %q", name, compiled.Name())
		}
	}
}

func TestEmptyResourcePolicyIncludesAllResourcesWithoutRules(t *testing.T) {
	compiled, err := LoadBytes([]byte(`apiVersion: kube-dump/v2
kind: Profile
metadata:
  name: empty-resources
  description: Empty resource policy test profile
resources: {}
`))
	if err != nil {
		t.Fatalf("LoadBytes() rejected an empty resource policy: %v", err)
	}

	if defaults := compiled.SelectionDefaults(); len(defaults.Resources) != 0 {
		t.Fatalf("empty resource policy has selection defaults: %#v", defaults.Resources)
	}

	object := testObject(state.Identity{
		Group: "apps", Version: "v1", Resource: "deployments", Kind: "Deployment", Name: "api",
	})
	if !compiled.AllowsResourceName(object.Identity.Name) {
		t.Fatal("empty resource policy rejected a discovered resource name")
	}
	if len(compiled.EncryptionPaths(object)) != 0 {
		t.Fatal("empty resource policy unexpectedly configured encryption")
	}
}

func TestBuiltInProfilesExcludeTechnicalImages(t *testing.T) {
	image := registry.Reference{
		Registry:   "registry.example",
		Repository: "platform/runner",
		Reference:  "latest",
	}
	tests := []struct {
		name   string
		labels map[string]string
	}{
		{
			name:   "GitLab Runner job",
			labels: map[string]string{"job.runner.gitlab.com/pod": "runner"},
		},
		{
			name:   "Tekton PipelineRun task",
			labels: map[string]string{"tekton.dev/pipelineRun": "pipeline-run"},
		},
		{
			name:   "Tekton standalone TaskRun",
			labels: map[string]string{"tekton.dev/taskRun": "task-run"},
		},
		{
			name:   "Flux controller",
			labels: map[string]string{"app.kubernetes.io/part-of": "flux"},
		},
		{
			name:   "GitHub Actions ephemeral runner",
			labels: map[string]string{"actions-ephemeral-runner": "True"},
		},
		{
			name:   "Argo Workflow",
			labels: map[string]string{"workflows.argoproj.io/workflow": "workflow"},
		},
	}

	for _, profileName := range []string{"backup", "export"} {
		compiled, err := Load(profileName)
		if err != nil {
			t.Fatalf("Load(%q) error = %v", profileName, err)
		}

		for _, test := range tests {
			t.Run(profileName+"/"+test.name, func(t *testing.T) {
				if compiled.MatchesImage(
					"ci", test.labels, nil,
					contract.ContainerTypeContainer, image,
				) {
					t.Fatalf("profile %q accepted %s image", profileName, test.name)
				}
			})
		}
	}

	raw, err := Load("raw")
	if err != nil {
		t.Fatalf("Load(%q) error = %v", "raw", err)
	}
	if !raw.MatchesImage("ci", tests[0].labels, nil, contract.ContainerTypeContainer, image) {
		t.Fatal("raw profile unexpectedly excluded a technical image")
	}
}

func TestResourceNameScopeSupportsOrderedNegativeGlobs(t *testing.T) {
	compiled, err := Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "negative-globs",
			Description: "test",
		},
		Resources: contract.ResourcePolicy{Selection: contract.ResourceSelectionSpec{
			ObjectScope: contract.ObjectScope{
				Names: []string{"*", "!generated-*", "generated-keep"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !compiled.AllowsResourceName("generated-keep") {
		t.Fatal("later positive name pattern did not re-include the object")
	}
	if compiled.AllowsResourceName("generated-drop") {
		t.Fatal("negative name pattern accepted an excluded object")
	}
}

func TestResolveFromPreservesYAMLFileExtension(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "custom.yaml")
	want := []byte("profile: test\n")
	if err := os.WriteFile(path, want, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := ResolveFrom(path, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("ResolveFrom() = %q, want %q", got, want)
	}
}

func TestBackupProfileSelectionAndEncryption(t *testing.T) {
	compiled, err := Load("backup")
	if err != nil {
		t.Fatal(err)
	}

	defaults := compiled.SelectionDefaults()
	if len(defaults.Resources) == 0 {
		t.Fatal("backup profile has no resource selection defaults")
	}

	secret := testObject(state.Identity{
		Version: "v1", Resource: "secrets", Kind: "Secret", Name: "credentials",
	})
	if len(compiled.EncryptionPaths(secret)) == 0 {
		t.Fatal("backup profile did not select Secret encryption paths")
	}

	paths := compiled.EncryptionPaths(secret)
	if len(paths) != 2 || paths[0][0] != "data" || paths[1][0] != "stringData" {
		t.Fatalf("backup Secret encryption paths = %#v", paths)
	}
}

func TestOrderedResourceSelection(t *testing.T) {
	compiled, err := Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "selection",
			Description: "test",
		},
		Resources: contract.ResourcePolicy{
			Selection: contract.ResourceSelectionSpec{
				Resources: []string{"*", "!core/v1/events"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	defaults := compiled.SelectionDefaults()
	if len(defaults.Resources) != 2 ||
		defaults.Resources[0] != "*" ||
		defaults.Resources[1] != "!core/v1/events" {
		t.Fatalf("selection defaults = %#v", defaults.Resources)
	}
}

func TestRuleMatchSupportsNamesLabelsAndAnnotations(t *testing.T) {
	compiled, err := Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "match",
			Description: "test",
		},
		Resources: contract.ResourcePolicy{
			Selection: contract.ResourceSelectionSpec{
				Resources: []string{"apps/v1/deployments"},
			},
			Rules: []contract.RuleSpec{{
				Name: "remove-api",
				Match: contract.MatchSpec{
					ObjectScope: contract.ObjectScope{
						Names: []string{"api"},
						LabelSelector: &contract.Selector{
							MatchLabels: map[string]string{"app": "api"},
						},
						AnnotationSelector: &contract.AnnotationSelector{
							MatchLabels: map[string]string{"backup": "true"},
						},
					},
					Resources: []string{"apps/v1/deployments"},
				},
				Remove: []string{".metadata.labels"},
			}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	object := testObject(state.Identity{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
		Kind:     "Deployment",
		Name:     "api",
	})
	object.Value.SetLabels(map[string]string{"app": "api"})
	object.Value.SetAnnotations(map[string]string{"backup": "true"})

	result, err := compiled.Apply(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(result.Value.Object, "metadata", "labels"); found {
		t.Fatal("matching remove rule did not remove the field")
	}
}

func TestProfileDecodesStructuredSelectors(t *testing.T) {
	data := []byte(`apiVersion: kube-dump/v2
kind: Profile
metadata:
  name: selectors
  description: Selector test profile
resources:
  selection:
    resources:
      - apps/v1/deployments
    labelSelector:
      matchLabels:
        app: api
  rules: []
`)
	if _, err := LoadBytes(data); err != nil {
		t.Fatalf("LoadBytes() rejected structured selector: %v", err)
	}
}

func TestProfileDecodesDomainSelections(t *testing.T) {
	data := []byte(`apiVersion: kube-dump/v2
kind: Profile
metadata:
  name: domain-selectors
  description: Domain selector test profile
resources:
  selection:
    resources:
      - apps/v1/deployments
  rules: []
pvc:
  selection:
    namespaces:
      - storage
images:
  selection:
    namespaces:
      - workloads
`)
	compiled, err := LoadBytes(data)
	if err != nil {
		t.Fatalf("LoadBytes() rejected domain selections: %v", err)
	}
	if got := compiled.PVCSelectionDefaults().Namespaces; len(got) != 1 || got[0] != "storage" {
		t.Fatalf("PVC selection namespaces = %#v", got)
	}
	if got := compiled.ImageSelectionDefaults().Namespaces; len(got) != 1 || got[0] != "workloads" {
		t.Fatalf("image selection namespaces = %#v", got)
	}
}

func TestDomainSelectionsMatchPVCAndImageMetadata(t *testing.T) {
	compiled, err := Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "domain-match",
			Description: "test",
		},
		PVC: &contract.PVCPolicy{Selection: contract.PVCSelectionSpec{
			ObjectScope: contract.ObjectScope{
				Names: []string{"selected-*"},
				LabelSelector: &contract.Selector{MatchLabels: map[string]string{
					"backup": "true",
				}},
				AnnotationSelector: &contract.AnnotationSelector{MatchExpressions: []contract.SelectorRequirement{
					{
						Key:      "managed",
						Operator: "Exists",
					},
				}},
			}}},
		Images: &contract.ImagePolicy{Selection: contract.ImageSelectionSpec{
			ObjectScope: contract.ObjectScope{
				LabelSelector: &contract.Selector{
					MatchLabels: map[string]string{"workload": "api"},
				},
			},
			Owners:         contract.OwnerSelectionSpec{},
			ContainerTypes: []contract.ContainerType{contract.ContainerTypeInitContainer},
			References: []contract.ImageReferenceSpec{{
				Registry:   "registry.example",
				Repository: "team/*",
				Tags:       []string{"release-*"},
			}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if !compiled.MatchesPVC(
		"storage", "selected-data",
		map[string]string{"backup": "true"},
		map[string]string{"managed": "controller"},
	) {
		t.Fatal("matching PVC was rejected")
	}
	if compiled.MatchesPVC(
		"storage", "other-data",
		map[string]string{"backup": "true"},
		map[string]string{"managed": "controller"},
	) {
		t.Fatal("PVC name filter was ignored")
	}
	if !compiled.MatchesPVCInScope(
		[]string{"storage"}, []string{"other-*"},
		"storage", "other-data",
		map[string]string{"backup": "true"},
		map[string]string{"managed": "controller"},
	) {
		t.Fatal("effective PVC name filter did not override profile defaults")
	}

	image := registry.Reference{
		Registry:   "registry.example",
		Repository: "team/api",
		Reference:  "release-1",
	}
	if !compiled.MatchesImage(
		"workloads",
		map[string]string{"workload": "api"}, nil,
		contract.ContainerTypeInitContainer, image,
	) {
		t.Fatal("matching image was rejected")
	}
	if compiled.MatchesImage(
		"workloads",
		map[string]string{"workload": "api"}, nil,
		contract.ContainerTypeContainer, image,
	) {
		t.Fatal("container type filter was ignored")
	}
	if compiled.MatchesImage(
		"workloads",
		map[string]string{"workload": "api"}, nil,
		contract.ContainerTypeInitContainer, registry.Reference{
			Registry: "registry.example", Repository: "team/api", Reference: "dev",
		},
	) {
		t.Fatal("image tag filter was ignored")
	}
}

func TestImageSelectionMatchesPodsAndOwners(t *testing.T) {
	compiled, err := Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "image-scopes",
			Description: "test",
		},
		Images: &contract.ImagePolicy{Selection: contract.ImageSelectionSpec{
			ObjectScope: contract.ObjectScope{
				Names: []string{"api-*"},
				LabelSelector: &contract.Selector{MatchExpressions: []contract.SelectorRequirement{
					{
						Key:      "job.runner.gitlab.com/pod",
						Operator: "DoesNotExist",
					},
				}},
			},
			Owners: contract.OwnerSelectionSpec{
				ObjectScope: contract.ObjectScope{LabelSelector: &contract.Selector{
					MatchLabels: map[string]string{"team": "platform"},
				}},
				Resources: []string{"apps/v1/deployments"},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	image := registry.Reference{
		Registry:   "ghcr.io",
		Repository: "platform/api",
		Reference:  "v1",
	}
	if !compiled.MatchesImagePodInNamespaces(
		nil, "ci", "api-0", map[string]string{}, nil, contract.ContainerTypeContainer, image,
	) {
		t.Fatal("matching Pod name was rejected before owner resolution")
	}
	if compiled.MatchesImagePodInNamespaces(
		nil, "ci", "worker-0", map[string]string{}, nil, contract.ContainerTypeContainer, image,
	) {
		t.Fatal("non-matching Pod name was accepted before owner resolution")
	}

	deployment := MatchObject{
		Group:     "apps",
		Version:   "v1",
		Resource:  "deployments",
		Namespace: "ci",
		Name:      "api",
		Labels:    map[string]string{"team": "platform"},
	}
	if !compiled.MatchesImageWithOwners(
		nil, "ci", "api-0", map[string]string{}, nil, contract.ContainerTypeContainer, image,
		[]MatchObject{deployment},
	) {
		t.Fatal("matching workload was rejected")
	}

	daemonSet := deployment
	daemonSet.Resource = "daemonsets"
	if compiled.MatchesImageWithOwners(
		nil, "ci", "api-0", nil, nil, contract.ContainerTypeContainer, image,
		[]MatchObject{daemonSet},
	) {
		t.Fatal("non-matching workload resource was accepted")
	}

	if compiled.MatchesImageWithOwners(
		nil, "ci", "api-0", map[string]string{"job.runner.gitlab.com/pod": "runner"}, nil,
		contract.ContainerTypeContainer, image, []MatchObject{deployment},
	) {
		t.Fatal("excluded Pod was accepted")
	}
}

func TestCompileRejectsRemoveAndEncryptInOneRule(t *testing.T) {
	_, err := Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "conflict",
			Description: "test",
		},
		Resources: contract.ResourcePolicy{
			Rules: []contract.RuleSpec{{
				Name:    "conflict",
				Remove:  []string{".data.password"},
				Encrypt: []string{".data.password"},
			}},
		},
	})
	if err == nil {
		t.Fatal("Compile() accepted a rule with remove and encrypt actions")
	}
}

func TestProfileRemovesFieldsWithoutMutatingInput(t *testing.T) {
	compiled, err := Load("backup")
	if err != nil {
		t.Fatal(err)
	}

	object := testObject(state.Identity{
		Version:  "v1",
		Resource: "configmaps",
		Kind:     "ConfigMap",
		Name:     "config",
	})
	object.Value.Object["status"] = map[string]any{"ready": true}
	object.Value.Object["metadata"].(map[string]any)["uid"] = "uid"

	result, err := compiled.Apply(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(result.Value.Object, "status"); found {
		t.Fatal("status was not removed")
	}
	if _, found, _ := unstructured.NestedFieldNoCopy(object.Value.Object, "status"); !found {
		t.Fatal("Apply mutated the input object")
	}
}

func TestProfileOmitsEmptyFieldsWithoutRemovingZeroValues(t *testing.T) {
	compiled, err := Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "omit-empty",
			Description: "test",
		},
		Resources: contract.ResourcePolicy{OmitEmpty: true},
	})
	if err != nil {
		t.Fatal(err)
	}

	object := testObject(state.Identity{
		Group:    "apps",
		Version:  "v1",
		Resource: "deployments",
		Kind:     "Deployment",
		Name:     "api",
	})
	object.Value.Object["spec"] = map[string]any{
		"replicas":  int64(0),
		"paused":    false,
		"command":   "",
		"emptyMap":  map[string]any{},
		"emptyList": []any{},
		"nested": map[string]any{
			"empty": map[string]any{},
		},
		"items": []any{map[string]any{"empty": map[string]any{}}},
		"null":  nil,
	}

	result, err := compiled.Apply(object)
	if err != nil {
		t.Fatal(err)
	}

	spec := result.Value.Object["spec"].(map[string]any)
	for _, key := range []string{"emptyMap", "emptyList", "nested", "null"} {
		if _, found := spec[key]; found {
			t.Errorf("empty field %q was retained: %#v", key, spec[key])
		}
	}
	for key, want := range map[string]any{
		"replicas": int64(0),
		"paused":   false,
		"command":  "",
	} {
		if got := spec[key]; !reflect.DeepEqual(got, want) {
			t.Errorf("spec[%q] = %#v, want %#v", key, got, want)
		}
	}

	items := spec["items"].([]any)
	if len(items) != 1 {
		t.Fatalf("empty list item was removed: %#v", items)
	}
	if got := items[0].(map[string]any); len(got) != 0 {
		t.Fatalf("empty list item was not compacted: %#v", got)
	}
}

func TestProfileKeepsEmptyFieldsByDefault(t *testing.T) {
	compiled, err := Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "keep-empty",
			Description: "test",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	object := testObject(state.Identity{
		Version: "v1", Resource: "configmaps", Kind: "ConfigMap", Name: "config",
	})
	object.Value.Object["data"] = map[string]any{}

	result, err := compiled.Apply(object)
	if err != nil {
		t.Fatal(err)
	}
	if _, found := result.Value.Object["data"]; !found {
		t.Fatal("empty field was removed without omitEmpty")
	}
}

func TestProfileNameDoesNotChangeNormalization(t *testing.T) {
	service := testObject(state.Identity{
		Version:  "v1",
		Resource: "services",
		Kind:     "Service",
		Name:     "api",
	})
	service.Value.Object["spec"] = map[string]any{
		"clusterIP":           "10.0.0.10",
		"clusterIPs":          []any{"10.0.0.10"},
		"ipFamilies":          []any{"IPv4"},
		"ipFamilyPolicy":      "SingleStack",
		"healthCheckNodePort": int64(30000),
		"ports": []any{map[string]any{
			"port":     int64(80),
			"nodePort": int64(30080),
		}},
	}

	compile := func(name string) *CompiledProfile {
		compiled, err := Compile(contract.Profile{
			APIVersion: "kube-dump/v2",
			Kind:       "Profile",
			Metadata: contract.Metadata{
				Name:        name,
				Description: "test",
			},
		})
		if err != nil {
			t.Fatalf("Compile(%q) error = %v", name, err)
		}

		return compiled
	}

	backupResult, err := compile("backup").Apply(service)
	if err != nil {
		t.Fatal(err)
	}

	rawResult, err := compile("raw").Apply(service)
	if err != nil {
		t.Fatal(err)
	}
	if backupResult.Value.Object["spec"].(map[string]any)["clusterIP"] != "10.0.0.10" {
		t.Fatal("profile unexpectedly changed Service networking state")
	}
	if !reflect.DeepEqual(backupResult.Value.Object, rawResult.Value.Object) {
		t.Fatal("profile name changed normalization result")
	}
}

func TestCompileRejectsInvalidRuleActions(t *testing.T) {
	base := func(rule contract.RuleSpec) contract.Profile {
		return contract.Profile{
			APIVersion: "kube-dump/v2",
			Kind:       "Profile",
			Metadata: contract.Metadata{
				Name:        "test",
				Description: "test",
			},
			Resources: contract.ResourcePolicy{
				Rules: []contract.RuleSpec{rule},
			},
		}
	}

	for name, rule := range map[string]contract.RuleSpec{
		"no action": {
			Match: contract.MatchSpec{},
		},
		"multiple actions": {
			Match:   contract.MatchSpec{},
			Remove:  []string{".metadata.name"},
			Encrypt: []string{".data"},
		},
		"invalid path": {
			Match:  contract.MatchSpec{},
			Remove: []string{"metadata.name"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Compile(base(rule)); err == nil {
				t.Fatal("Compile() accepted invalid rule")
			}
		})
	}
}

func TestCompileAcceptsGenericEncryption(t *testing.T) {
	_, err := Compile(contract.Profile{
		APIVersion: "kube-dump/v2",
		Kind:       "Profile",
		Metadata: contract.Metadata{
			Name:        "test",
			Description: "test",
		},
		Resources: contract.ResourcePolicy{
			Rules: []contract.RuleSpec{{
				Match: contract.MatchSpec{
					Resources: []string{"apps/v1/deployments"},
				},
				Encrypt: []string{".spec.password"},
			}}},
	})
	if err != nil {
		t.Fatalf("Compile() error = %v", err)
	}
}

func TestBuiltinsExposeDescriptions(t *testing.T) {
	infos, err := BuiltinInfos()
	if err != nil || len(infos) != 3 || infos[0].Description == "" {
		t.Fatalf("BuiltinInfos() = %#v, error = %v", infos, err)
	}
}

// testObject builds a minimal state object from the identity used by a profile test.
func testObject(identity state.Identity) state.Object {
	apiVersion := identity.Version
	if identity.Group != "" {
		apiVersion = identity.Group + "/" + apiVersion
	}

	metadata := map[string]any{"name": identity.Name}
	if identity.Namespace != "" {
		metadata["namespace"] = identity.Namespace
	}

	object, err := state.NewObject(identity, &unstructured.Unstructured{
		Object: map[string]any{
			"apiVersion": apiVersion,
			"kind":       identity.Kind,
			"metadata":   metadata,
		},
	})
	if err != nil {
		panic(err)
	}

	return object
}
