// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import "testing"

func TestParseResourceExpressionCanonicalForms(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name  string
		value string
		want  ResourceExpression
	}{
		{
			name:  "bare resource",
			value: "deployments:api",
			want: ResourceExpression{
				Group:    "*",
				Version:  "*",
				Resource: "deployments",
				Name:     "api",
			},
		},
		{
			name:  "core short form",
			value: "v1/configmaps",
			want: ResourceExpression{
				Group:    "core",
				Version:  "v1",
				Resource: "configmaps",
			},
		},
		{
			name:  "qualified resource",
			value: "apps/v1/deployments:api",
			want: ResourceExpression{
				Group:    "apps",
				Version:  "v1",
				Resource: "deployments",
				Name:     "api",
			},
		},
		{
			name:  "excluded wildcard",
			value: "!*",
			want: ResourceExpression{
				Group:    "*",
				Version:  "*",
				Resource: "*",
				Exclude:  true,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ParseResourceExpression(test.value)
			if err != nil {
				t.Fatalf("ParseResourceExpression() error = %v", err)
			}
			if got != test.want {
				t.Fatalf("ParseResourceExpression() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestParseResourceExpressionRejectsMalformedValues(t *testing.T) {
	t.Parallel()

	for _, value := range []string{
		"", "!", "v1",
		"apps//deployments",
		"apps/v1/deployments:",
		"apps/v1/deployments:api:extra",
		"apps/v1/deployments/extra",
	} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseResourceExpression(value); err == nil {
				t.Fatalf("ParseResourceExpression(%q) accepted malformed value", value)
			}
		})
	}
}

func TestResourceExpressionMatchesObjectAndResource(t *testing.T) {
	t.Parallel()

	expression, err := ParseResourceExpression("v1/configmaps:settings")
	if err != nil {
		t.Fatal(err)
	}

	if !expression.MatchesResource("", "v1", "configmaps") {
		t.Fatal("core short form did not match the core resource")
	}
	if expression.MatchesResource("apps", "v1", "configmaps") {
		t.Fatal("core short form matched a non-core resource")
	}
	if !expression.Matches("", "v1", "configmaps", "settings") {
		t.Fatal("resource expression did not match the selected object")
	}
	if expression.Matches("", "v1", "configmaps", "other") {
		t.Fatal("resource expression matched an unrelated object")
	}
}
