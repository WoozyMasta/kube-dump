// SPDX-License-Identifier: MIT
// SPDX-FileCopyrightText: Copyright 2026 WoozyMasta
// Source: https://github.com/WoozyMasta/kube-dump

package profile

import (
	"encoding/json"
	"fmt"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"
	contract "github.com/woozymasta/kube-dump/v2/pkg/profile"
	profileschema "github.com/woozymasta/kube-dump/v2/pkg/profile/schema"
)

var (
	compiledSchemaOnce sync.Once
	compiledSchema     *jsonschema.Schema
	compiledSchemaErr  error
)

// validateSchema checks the decoded profile against the generated public contract
// before semantic compilation applies runtime-only rules.
func validateSchema(document contract.Profile) error {
	data, err := json.Marshal(document)
	if err != nil {
		return fmt.Errorf("marshal profile for schema validation: %w", err)
	}

	compiled, err := profileSchema()
	if err != nil {
		return err
	}

	var value any
	if err := json.Unmarshal(data, &value); err != nil {
		return fmt.Errorf("decode profile for schema validation: %w", err)
	}
	if err := compiled.Validate(value); err != nil {
		return fmt.Errorf("profile does not match schema: %w", err)
	}

	return nil
}

// profileSchema compiles the embedded schema once and shares it across loads.
func profileSchema() (*jsonschema.Schema, error) {
	compiledSchemaOnce.Do(func() {
		compiler := jsonschema.NewCompiler()
		const resourceURL = "urn:kube-dump:profile:v1"
		var document any
		if err := json.Unmarshal(profileschema.JSON(), &document); err != nil {
			compiledSchemaErr = fmt.Errorf("decode embedded profile schema: %w", err)
			return
		}

		if err := compiler.AddResource(resourceURL, document); err != nil {
			compiledSchemaErr = fmt.Errorf("add embedded profile schema: %w", err)
			return
		}

		compiledSchema, compiledSchemaErr = compiler.Compile(resourceURL)
		if compiledSchemaErr != nil {
			compiledSchemaErr = fmt.Errorf("compile embedded profile schema: %w", compiledSchemaErr)
		}
	})

	return compiledSchema, compiledSchemaErr
}
