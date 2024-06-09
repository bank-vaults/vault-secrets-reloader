// Copyright © 2023 Cisco
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package reloader

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestIncrementReloadCountAnnotation(t *testing.T) {
	tests := []struct {
		name              string
		annotation        string
		annotationValue   string
		expectedAnnoation string
		expectedValue     string
	}{
		{
			name:              "no annotation should use the ReloadCountAnnotationName",
			annotation:        "",
			annotationValue:   "",
			expectedAnnoation: ReloadCountAnnotationName,
			expectedValue:     "1",
		},
		{
			name:              "declared annotation should use the same annotation",
			annotation:        ReloadCountAnnotationName,
			annotationValue:   "1",
			expectedAnnoation: ReloadCountAnnotationName,
			expectedValue:     "2",
		},
		{
			name:              "deprecated annotation should use the same annotation",
			annotation:        DeprecatedReloadCountAnnotationName,
			annotationValue:   "1",
			expectedAnnoation: DeprecatedReloadCountAnnotationName,
			expectedValue:     "2",
		},
	}

	for _, tt := range tests {
		ttp := tt
		t.Run(ttp.name, func(t *testing.T) {
			annotations := map[string]string{}
			if ttp.annotation != "" {
				annotations[ttp.annotation] = ttp.annotationValue
			}

			podTemplateSpec := &corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: annotations,
				},
			}

			incrementReloadCountAnnotation(podTemplateSpec)

			annotationValue, ok := podTemplateSpec.Annotations[ttp.expectedAnnoation]
			assert.True(t, ok)
			assert.Equal(t, ttp.expectedValue, annotationValue)
		})
	}
}
