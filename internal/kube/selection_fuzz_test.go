// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package kube

import (
	"strings"
	"testing"

	"github.com/woozymasta/kube-dump/v2/internal/state"
)

func FuzzResourceSelection(f *testing.F) {
	f.Add("apps", "v1", "deployments", "api", "production")
	f.Add("", "v1", "configmaps", "settings", "default")

	f.Fuzz(func(t *testing.T, group, version, resource, name, namespace string) {
		const maxSelectionPart = 256
		if len(group) > maxSelectionPart ||
			len(version) > maxSelectionPart ||
			len(resource) > maxSelectionPart ||
			len(name) > maxSelectionPart ||
			len(namespace) > maxSelectionPart {
			return
		}
		if strings.ContainsAny(version, "/:") ||
			strings.ContainsAny(group, "/:") ||
			strings.ContainsAny(resource, "/:") ||
			strings.ContainsAny(name, "/:") ||
			strings.ContainsAny(namespace, "/") {
			return
		}
		if strings.HasPrefix(group, "!") {
			return
		}
		if group == "" || version == "" || resource == "" || name == "" || namespace == "" {
			return
		}

		identity := state.Identity{
			Group:     group,
			Version:   version,
			Resource:  resource,
			Name:      name,
			Namespace: namespace,
		}
		expression := group + "/" + version + "/" + resource

		if !(Selection{}).AllowsIdentity(identity) {
			t.Fatal("empty resource selection rejected an identity")
		}
		if !(Selection{Resources: []string{"*"}}).AllowsIdentity(identity) {
			t.Fatal("wildcard resource selection rejected an identity")
		}

		excluded := Selection{Resources: []string{"*", "!" + expression + ":" + name}}
		if excluded.AllowsIdentity(identity) {
			t.Fatal("matching resource exclusion did not reject an identity")
		}

		reincluded := Selection{Resources: []string{"*", "!" + expression + ":" + name, expression}}
		if !reincluded.AllowsIdentity(identity) {
			t.Fatal("later matching resource expression did not restore an identity")
		}

		selected := Selection{Namespaces: []string{namespace}}
		selected.ExcludeNamespaces = []string{namespace}
		if selected.NamespaceSelected(namespace) {
			t.Fatal("namespace exclusion did not override namespace inclusion")
		}
	})
}
