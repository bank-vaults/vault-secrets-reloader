// Copyright © 2026 Contributors
// SPDX-License-Identifier: Apache-2.0
package tests

import (
	"context"
	"k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	schemaCEL "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"os"
	"testing"

	extensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	extensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	validation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	"sigs.k8s.io/yaml"
)

func TestCRDSchemaOfflineValidation(t *testing.T) {
	b, err := os.ReadFile("../config/crd/bases/secrets-reloader.security.bank-vaults.io_vaultreloaders.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v1 extensionsv1.CustomResourceDefinition
	if err = yaml.Unmarshal(b, &v1); err != nil {
		t.Fatal(err)
	}
	extensionsv1.SetDefaults_CustomResourceDefinition(&v1)
	var crd extensions.CustomResourceDefinition
	if err = extensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(&v1, &crd, nil); err != nil {
		t.Fatal(err)
	}
	crd.Status.StoredVersions = []string{"v1alpha1"}
	if errs := validation.ValidateCustomResourceDefinition(context.Background(), &crd); len(errs) > 0 {
		t.Fatalf("invalid CRD: %v", errs)
	}
}

func TestCRDRejectsNonDefaultName(t *testing.T) {
	b, err := os.ReadFile("../config/crd/bases/secrets-reloader.security.bank-vaults.io_vaultreloaders.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var v1 extensionsv1.CustomResourceDefinition
	if err = yaml.Unmarshal(b, &v1); err != nil {
		t.Fatal(err)
	}
	var props extensions.JSONSchemaProps
	if err = extensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(v1.Spec.Versions[0].Schema.OpenAPIV3Schema, &props, nil); err != nil {
		t.Fatal(err)
	}
	structural, err := schema.NewStructural(&props)
	if err != nil {
		t.Fatal(err)
	}
	validator := schemaCEL.NewValidator(structural, true, 100000)
	if validator == nil {
		t.Fatal("CEL singleton validation missing")
	}
	for _, name := range []string{"default", "other"} {
		obj := map[string]interface{}{"apiVersion": "secrets-reloader.security.bank-vaults.io/v1alpha1", "kind": "VaultReloader", "metadata": map[string]interface{}{"name": name, "namespace": "team-a"}, "spec": map[string]interface{}{"suspend": false}}
		errs, _ := validator.Validate(context.Background(), field.NewPath("root"), structural, obj, nil, 100000)
		if name == "default" && len(errs) > 0 {
			t.Fatal(errs)
		}
		if name != "default" && len(errs) == 0 {
			t.Fatal("non-default name accepted")
		}
	}
}
