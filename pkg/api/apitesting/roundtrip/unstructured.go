/*
Copyright 2024 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package roundtrip

import (
	"bytes"
	"fmt"
	"math/rand"
	"os"
	"strconv"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/apitesting/fuzzer"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	cborserializer "k8s.io/apimachinery/pkg/runtime/serializer/cbor"
	cbor "k8s.io/apimachinery/pkg/runtime/serializer/cbor/direct"
	jsonserializer "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/util/sets"

	"github.com/google/go-cmp/cmp"
)

// RoundtripToUnstructured verifies the roundtrip faithfulness of all external types in a scheme
// from native to unstructured and back using both the JSON and CBOR serializers. The intermediate
// unstructured objects produced by both encodings must be identical and be themselves
// roundtrippable to JSON and CBOR.
func RoundtripToUnstructured(t *testing.T, scheme *runtime.Scheme, funcs fuzzer.FuzzerFuncs, skipped sets.Set[schema.GroupVersionKind]) {
	codecs := serializer.NewCodecFactory(scheme)

	seed := int64(time.Now().Nanosecond())
	if override := os.Getenv("TEST_RAND_SEED"); len(override) > 0 {
		overrideSeed, err := strconv.ParseInt(override, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		seed = overrideSeed
		t.Logf("using overridden seed: %d", seed)
	} else {
		t.Logf("seed (override with TEST_RAND_SEED if desired): %d", seed)
	}

	var buf bytes.Buffer
	for gvk := range scheme.AllKnownTypes() {
		if globalNonRoundTrippableTypes.Has(gvk.Kind) {
			continue
		}
		if gvk.Version == runtime.APIVersionInternal {
			continue
		}

		subtestName := fmt.Sprintf("%s.%s/%s", gvk.Version, gvk.Group, gvk.Kind)
		if gvk.Group == "" {
			subtestName = fmt.Sprintf("%s/%s", gvk.Version, gvk.Kind)
		}

		t.Run(subtestName, func(t *testing.T) {
			if skipped.Has(gvk) {
				t.Skip()
			}

			fuzzer := fuzzer.FuzzerFor(funcs, rand.NewSource(seed), codecs)

			for i := 0; i < 50; i++ {
				// We do fuzzing on the internal version of the object, and only then
				// convert to the external version. This is because custom fuzzing
				// function are only supported for internal objects.
				internalObj, err := scheme.New(schema.GroupVersion{Group: gvk.Group, Version: runtime.APIVersionInternal}.WithKind(gvk.Kind))
				if err != nil {
					t.Fatalf("couldn't create internal object %v: %v", gvk.Kind, err)
				}
				fuzzer.Fuzz(internalObj)

				item, err := scheme.New(gvk)
				if err != nil {
					t.Fatalf("couldn't create external object %v: %v", gvk.Kind, err)
				}
				if err := scheme.Convert(internalObj, item, nil); err != nil {
					t.Fatalf("conversion for %v failed: %v", gvk.Kind, err)
				}

				// Decoding into Unstructured requires that apiVersion and kind be
				// serialized, so populate TypeMeta.
				item.GetObjectKind().SetGroupVersionKind(gvk)

				jsonSerializer := jsonserializer.NewSerializerWithOptions(jsonserializer.DefaultMetaFactory, scheme, scheme, jsonserializer.SerializerOptions{})
				cborSerializer := cborserializer.NewSerializer(scheme, scheme)

				// original->JSON->Unstructured
				buf.Reset()
				if err := jsonSerializer.Encode(item, &buf); err != nil {
					t.Fatalf("error encoding native to json: %v", err)
				}
				var uJSON runtime.Object = &unstructured.Unstructured{}
				uJSON, _, err = jsonSerializer.Decode(buf.Bytes(), &gvk, uJSON)
				if err != nil {
					t.Fatalf("error decoding json to unstructured: %v", err)
				}

				// original->CBOR->Unstructured
				buf.Reset()
				if err := cborSerializer.Encode(item, &buf); err != nil {
					t.Fatalf("error encoding native to cbor: %v", err)
				}
				var uCBOR runtime.Object = &unstructured.Unstructured{}
				uCBOR, _, err = cborSerializer.Decode(buf.Bytes(), &gvk, uCBOR)
				if err != nil {
					diag, _ := cbor.Diagnose(buf.Bytes())
					t.Fatalf("error decoding cbor to unstructured: %v, diag: %s", err, diag)
				}

				// original->JSON->Unstructured == original->CBOR->Unstructured
				if !apiequality.Semantic.DeepEqual(uJSON, uCBOR) {
					t.Fatalf("unstructured via json differed from unstructured via cbor: %v", cmp.Diff(uJSON, uCBOR))
				}

				// original->CBOR(nondeterministic)->Unstructured
				buf.Reset()
				if err := cborSerializer.EncodeNondeterministic(item, &buf); err != nil {
					t.Fatalf("error encoding native to cbor: %v", err)
				}
				var uCBORNondeterministic runtime.Object = &unstructured.Unstructured{}
				uCBORNondeterministic, _, err = cborSerializer.Decode(buf.Bytes(), &gvk, uCBORNondeterministic)
				if err != nil {
					diag, _ := cbor.Diagnose(buf.Bytes())
					t.Fatalf("error decoding cbor to unstructured: %v, diag: %s", err, diag)
				}

				// original->CBOR->Unstructured == original->CBOR(nondeterministic)->Unstructured
				if !apiequality.Semantic.DeepEqual(uCBOR, uCBORNondeterministic) {
					t.Fatalf("unstructured via nondeterministic cbor differed from unstructured via cbor: %v", cmp.Diff(uCBOR, uCBORNondeterministic))
				}

				// original->JSON/CBOR->Unstructured == original->JSON/CBOR->Unstructured->JSON->Unstructured
				buf.Reset()
				if err := jsonSerializer.Encode(uJSON, &buf); err != nil {
					t.Fatalf("error encoding unstructured to json: %v", err)
				}
				var uJSON2 runtime.Object = &unstructured.Unstructured{}
				uJSON2, _, err = jsonSerializer.Decode(buf.Bytes(), &gvk, uJSON2)
				if err != nil {
					t.Fatalf("error decoding json to unstructured: %v", err)
				}
				if !apiequality.Semantic.DeepEqual(uJSON, uJSON2) {
					t.Errorf("object changed during native-json-unstructured-json-unstructured roundtrip, diff: %s", cmp.Diff(uJSON, uJSON2))
				}

				// original->JSON/CBOR->Unstructured == original->JSON/CBOR->Unstructured->CBOR->Unstructured
				buf.Reset()
				if err := cborSerializer.Encode(uCBOR, &buf); err != nil {
					t.Fatalf("error encoding unstructured to cbor: %v", err)
				}
				var uCBOR2 runtime.Object = &unstructured.Unstructured{}
				uCBOR2, _, err = cborSerializer.Decode(buf.Bytes(), &gvk, uCBOR2)
				if err != nil {
					diag, _ := cbor.Diagnose(buf.Bytes())
					t.Fatalf("error decoding cbor to unstructured: %v, diag: %s", err, diag)
				}
				if !apiequality.Semantic.DeepEqual(uCBOR, uCBOR2) {
					t.Errorf("object changed during native-cbor-unstructured-cbor-unstructured roundtrip, diff: %s", cmp.Diff(uCBOR, uCBOR2))
				}

				// original->JSON/CBOR->Unstructured->CBOR->Unstructured == original->JSON/CBOR->Unstructured->CBOR(nondeterministic)->Unstructured
				buf.Reset()
				if err := cborSerializer.EncodeNondeterministic(uCBOR, &buf); err != nil {
					t.Fatalf("error encoding unstructured to cbor: %v", err)
				}
				var uCBOR2Nondeterministic runtime.Object = &unstructured.Unstructured{}
				uCBOR2Nondeterministic, _, err = cborSerializer.Decode(buf.Bytes(), &gvk, uCBOR2Nondeterministic)
				if err != nil {
					diag, _ := cbor.Diagnose(buf.Bytes())
					t.Fatalf("error decoding cbor to unstructured: %v, diag: %s", err, diag)
				}
				if !apiequality.Semantic.DeepEqual(uCBOR, uCBOR2Nondeterministic) {
					t.Errorf("object changed during native-cbor-unstructured-cbor(nondeterministic)-unstructured roundtrip, diff: %s", cmp.Diff(uCBOR, uCBOR2Nondeterministic))
				}

				// original->JSON/CBOR->Unstructured->JSON->final == original
				buf.Reset()
				if err := jsonSerializer.Encode(uJSON, &buf); err != nil {
					t.Fatalf("error encoding unstructured to json: %v", err)
				}
				finalJSON, _, err := jsonSerializer.Decode(buf.Bytes(), &gvk, nil)
				if err != nil {
					t.Fatalf("error decoding json to native: %v", err)
				}
				if !apiequality.Semantic.DeepEqual(item, finalJSON) {
					t.Errorf("object changed during native-json-unstructured-json-native roundtrip, diff: %s", cmp.Diff(item, finalJSON))
				}

				// original->JSON/CBOR->Unstructured->CBOR->final == original
				buf.Reset()
				if err := cborSerializer.Encode(uCBOR, &buf); err != nil {
					t.Fatalf("error encoding unstructured to cbor: %v", err)
				}
				finalCBOR, _, err := cborSerializer.Decode(buf.Bytes(), &gvk, nil)
				if err != nil {
					diag, _ := cbor.Diagnose(buf.Bytes())
					t.Fatalf("error decoding cbor to native: %v, diag: %s", err, diag)
				}
				if !apiequality.Semantic.DeepEqual(item, finalCBOR) {
					t.Errorf("object changed during native-cbor-unstructured-cbor-native roundtrip, diff: %s", cmp.Diff(item, finalCBOR))
				}

				// original->JSON/CBOR->Unstructured->CBOR(nondeterministic)->final == original
				buf.Reset()
				if err := cborSerializer.EncodeNondeterministic(uCBOR, &buf); err != nil {
					t.Fatalf("error encoding unstructured to cbor: %v", err)
				}
				finalCBORNondeterministic, _, err := cborSerializer.Decode(buf.Bytes(), &gvk, nil)
				if err != nil {
					diag, _ := cbor.Diagnose(buf.Bytes())
					t.Fatalf("error decoding cbor to native: %v, diag: %s", err, diag)
				}
				if !apiequality.Semantic.DeepEqual(item, finalCBORNondeterministic) {
					t.Errorf("object changed during native-cbor-unstructured-cbor-native roundtrip, diff: %s", cmp.Diff(item, finalCBORNondeterministic))
				}
			}
		})
	}
}
