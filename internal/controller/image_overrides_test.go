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

package controller

import (
	"fmt"
	"testing"

	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func TestFixImages(t *testing.T) {
	type args struct {
		objs           []unstructured.Unstructured
		alterImageFunc func(image string) (string, error)
	}

	tests := []struct {
		name    string
		args    args
		want    []string
		wantErr bool
	}{
		{
			name: "fix deployment containers images",
			args: args{
				objs: []unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"apiVersion": "apps/v1",
							"kind":       deploymentKind,
							"spec": map[string]interface{}{
								"template": map[string]interface{}{
									"spec": map[string]interface{}{
										"containers": []map[string]interface{}{
											{
												"image": "container-image",
											},
										},
										"initContainers": []map[string]interface{}{
											{
												"image": "init-container-image",
											},
										},
									},
								},
							},
						},
					},
				},
				alterImageFunc: func(image string) (string, error) {
					return fmt.Sprintf("foo-%s", image), nil
				},
			},
			want:    []string{"foo-container-image", "foo-init-container-image"},
			wantErr: false,
		},
		{
			name: "fix daemonSet containers images",
			args: args{
				objs: []unstructured.Unstructured{
					{
						Object: map[string]interface{}{
							"apiVersion": "apps/v1",
							"kind":       daemonSetKind,
							"spec": map[string]interface{}{
								"template": map[string]interface{}{
									"spec": map[string]interface{}{
										"containers": []map[string]interface{}{
											{
												"image": "container-image",
											},
										},
										"initContainers": []map[string]interface{}{
											{
												"image": "init-container-image",
											},
										},
									},
								},
							},
						},
					},
				},
				alterImageFunc: func(image string) (string, error) {
					return fmt.Sprintf("foo-%s", image), nil
				},
			},
			want:    []string{"foo-container-image", "foo-init-container-image"},
			wantErr: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			g := NewWithT(t)

			got, err := fixImages(tt.args.objs, tt.args.alterImageFunc)
			if tt.wantErr {
				g.Expect(err).To(HaveOccurred())
				return
			}

			g.Expect(err).ToNot(HaveOccurred())

			gotImages, err := inspectImages(got)
			g.Expect(err).ToNot(HaveOccurred())
			g.Expect(gotImages).To(Equal(tt.want))
		})
	}
}
