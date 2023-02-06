package controllers

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/fluxcd/pkg/apis/meta"
	sourcev1 "github.com/fluxcd/source-controller/api/v1beta2"
	"github.com/onsi/gomega/ghttp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	"github.com/open-component-model/ocm-controller/api/v1alpha1"
	cachefakes "github.com/open-component-model/ocm-controller/pkg/cache/fakes"
	"github.com/open-component-model/ocm-controller/pkg/ocm/fakes"
)

var configurationConfigData = []byte(`kind: ConfigData
metadata:
  name: test-config-data
  namespace: default
configuration:
  defaults:
    color: red
    message: Hello, world!
  schema:
    type: object
    additionalProperties: false
    properties:
      color:
        type: string
      message:
        type: string
  rules:
  - value: (( message ))
    file: configmap.yaml
    path: data.PODINFO_UI_MESSAGE
  - value: (( color ))
    file: configmap.yaml
    path: data.PODINFO_UI_COLOR
`)

type configurationTestCase struct {
	name                string
	mock                func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher)
	componentVersion    func() *v1alpha1.ComponentVersion
	componentDescriptor func() *v1alpha1.ComponentDescriptor
	snapshot            func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot
	source              func(snapshot *v1alpha1.Snapshot) v1alpha1.Source
	patchStrategicMerge func(cfg *v1alpha1.Configuration, source client.Object, filename string) *v1alpha1.Configuration
	expectError         string
}

func TestConfigurationReconciler(t *testing.T) {
	testCases := []configurationTestCase{
		{
			name: "with snapshot as a source",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				return v1alpha1.Source{
					SourceRef: &meta.NamespacedObjectKindReference{
						APIVersion: v1alpha1.GroupVersion.String(),
						Kind:       "Snapshot",
						Name:       snapshot.Name,
						Namespace:  snapshot.Namespace,
					},
				}
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				identity := v1alpha1.Identity{
					v1alpha1.ComponentNameKey:    cv.Spec.Component,
					v1alpha1.ComponentVersionKey: cv.Status.ReconciledVersion,
					v1alpha1.ResourceNameKey:     resource.Spec.Resource.Name,
					v1alpha1.ResourceVersionKey:  resource.Spec.Resource.Version,
				}
				sourceSnapshot := &v1alpha1.Snapshot{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-snapshot",
						Namespace: cv.Namespace,
					},
					Spec: v1alpha1.SnapshotSpec{
						Identity: identity,
					},
				}
				return sourceSnapshot
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				content, err := os.Open(filepath.Join("testdata", "configuration-map.tar"))
				require.NoError(t, err)
				fakeCache.FetchDataByDigestReturns(content, nil)
				fakeOcm.GetResourceReturns(io.NopCloser(bytes.NewBuffer(configurationConfigData)), "", nil)
			},
		},
		{
			name: "with resource as a source",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				// do nothing
				return nil
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				// do nothing
				return v1alpha1.Source{
					ResourceRef: &v1alpha1.ResourceRef{
						Name:    "some-resource",
						Version: "1.0.0",
					},
				}
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				content, err := os.Open(filepath.Join("testdata", "configuration-map.tar"))
				require.NoError(t, err)
				fakeOcm.GetResourceReturnsOnCall(0, content, nil)
				fakeOcm.GetResourceReturnsOnCall(1, io.NopCloser(bytes.NewBuffer(configurationConfigData)), nil)
			},
		},
		{
			name:        "expect error when neither source or resource ref is defined as a source",
			expectError: "either sourceRef or resourceRef should be defined, but both are empty",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				// do nothing
				return nil
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				// do nothing
				return v1alpha1.Source{}
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				content, err := os.Open(filepath.Join("testdata", "configuration-map.tar"))
				require.NoError(t, err)
				fakeOcm.GetResourceReturns(content, "", nil)
			},
		},
		{
			name:        "expect error when get resource fails with snapshot",
			expectError: "failed to fetch resource data from snapshot: failed to fetch data: boo",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				return v1alpha1.Source{
					SourceRef: &meta.NamespacedObjectKindReference{
						APIVersion: v1alpha1.GroupVersion.String(),
						Kind:       "Snapshot",
						Name:       snapshot.Name,
						Namespace:  snapshot.Namespace,
					},
				}
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				identity := v1alpha1.Identity{
					v1alpha1.ComponentNameKey:    cv.Spec.Component,
					v1alpha1.ComponentVersionKey: cv.Status.ReconciledVersion,
					v1alpha1.ResourceNameKey:     resource.Spec.Resource.Name,
					v1alpha1.ResourceVersionKey:  resource.Spec.Resource.Version,
				}
				sourceSnapshot := &v1alpha1.Snapshot{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-snapshot",
						Namespace: cv.Namespace,
					},
					Spec: v1alpha1.SnapshotSpec{
						Identity: identity,
					},
				}
				return sourceSnapshot
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				fakeCache.FetchDataByDigestReturns(nil, errors.New("boo"))
			},
		},
		{
			name:        "expect error when get resource fails without snapshots",
			expectError: "failed to fetch resource data from resource ref: failed to fetch resource from resource ref: boo",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				// do nothing
				return nil
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				// do nothing
				return v1alpha1.Source{
					ResourceRef: &v1alpha1.ResourceRef{
						Name:    "some-resource",
						Version: "1.0.0",
					},
				}
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				fakeOcm.GetResourceReturns(nil, "digest", errors.New("boo"))
			},
		},
		{
			name:        "get resource fails during config data fetch",
			expectError: "failed to get resource: boo",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				// do nothing
				return nil
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				// do nothing
				return v1alpha1.Source{
					ResourceRef: &v1alpha1.ResourceRef{
						Name:    "some-resource",
						Version: "1.0.0",
					},
				}
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				content, err := os.Open(filepath.Join("testdata", "configuration-map.tar"))
				require.NoError(t, err)
				fakeOcm.GetResourceReturnsOnCall(0, content, nil)
				fakeOcm.GetResourceReturnsOnCall(1, nil, errors.New("boo"))
			},
		},
		{
			name:        "error while running configurator",
			expectError: "configurator error: error while doing cascade with: processing template adjustments: unresolved nodes:\n\t(( nope ))\tin template adjustments\tadjustments.[0].value\t(adjustments.name:subst-0.value)\t*'nope' not found",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				// do nothing
				return nil
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				// do nothing
				return v1alpha1.Source{
					ResourceRef: &v1alpha1.ResourceRef{
						Name:    "some-resource",
						Version: "1.0.0",
					},
				}
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				content, err := os.Open(filepath.Join("testdata", "configuration-map.tar"))
				require.NoError(t, err)
				fakeOcm.GetResourceReturnsOnCall(0, content, nil)
				testConfigData := []byte(`kind: ConfigData
metadata:
  name: test-config-data
  namespace: default
configuration:
  defaults:
    color: red
    message: Hello, world!
  schema:
    type: object
    additionalProperties: false
    properties:
      color:
        type: string
      message:
        type: string
  rules:
  - value: (( nope ))
    file: configmap.yaml
    path: data.PODINFO_UI_MESSAGE
  - value: (( color ))
    file: configmap.yaml
    path: data.PODINFO_UI_COLOR
`)
				fakeOcm.GetResourceReturnsOnCall(1, io.NopCloser(bytes.NewBuffer(testConfigData)), nil)
			},
		},
		{
			name:        "configuration fails because the file does not exist",
			expectError: "no such file or directory",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				// do nothing
				return nil
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				// do nothing
				return v1alpha1.Source{
					ResourceRef: &v1alpha1.ResourceRef{
						Name:    "some-resource",
						Version: "1.0.0",
					},
				}
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				content, err := os.Open(filepath.Join("testdata", "configuration-map.tar"))
				require.NoError(t, err)
				fakeOcm.GetResourceReturnsOnCall(0, content, nil)
				testConfigData := []byte(`kind: ConfigData
metadata:
  name: test-config-data
  namespace: default
configuration:
  defaults:
    color: red
    message: Hello, world!
  schema:
    type: object
    additionalProperties: false
    properties:
      color:
        type: string
      message:
        type: string
  rules:
  - value: (( message ))
    file: nope.yaml
    path: data.PODINFO_UI_MESSAGE
  - value: (( color ))
    file: configmap.yaml
    path: data.PODINFO_UI_COLOR
`)
				fakeOcm.GetResourceReturnsOnCall(1, io.NopCloser(bytes.NewBuffer(testConfigData)), nil)
			},
		},
		{
			name:        "it fails to marshal the config data",
			expectError: "failed to unmarshal content",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				// do nothing
				return nil
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				// do nothing
				return v1alpha1.Source{
					ResourceRef: &v1alpha1.ResourceRef{
						Name:    "some-resource",
						Version: "1.0.0",
					},
				}
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				content, err := os.Open(filepath.Join("testdata", "localization-deploy.tar"))
				require.NoError(t, err)
				fakeOcm.GetResourceReturnsOnCall(0, content, nil)
				testConfigData := []byte(`iaminvalidyaml`)
				fakeOcm.GetResourceReturnsOnCall(1, io.NopCloser(bytes.NewBuffer(testConfigData)), nil)
			},
		},
		{
			name:        "it fails to push data to the cache",
			expectError: "failed to push blob to local registry: boo",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				// do nothing
				return nil
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				// do nothing
				return v1alpha1.Source{
					ResourceRef: &v1alpha1.ResourceRef{
						Name:    "some-resource",
						Version: "1.0.0",
					},
				}
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				content, err := os.Open(filepath.Join("testdata", "configuration-map.tar"))
				require.NoError(t, err)
				fakeCache.PushDataReturns("", errors.New("boo"))
				fakeOcm.GetResourceReturnsOnCall(0, content, nil)
				fakeOcm.GetResourceReturnsOnCall(1, io.NopCloser(bytes.NewBuffer(configurationConfigData)), nil)
			},
		},
		{
			name: "it performs a strategic merge",
			componentVersion: func() *v1alpha1.ComponentVersion {
				cv := DefaultComponent.DeepCopy()
				cv.Status.ComponentDescriptor = v1alpha1.Reference{
					Name:    "test-component",
					Version: "v0.0.1",
					ComponentDescriptorRef: meta.NamespacedObjectReference{
						Name:      cv.Name + "-descriptor",
						Namespace: cv.Namespace,
					},
				}
				return cv
			},
			componentDescriptor: func() *v1alpha1.ComponentDescriptor {
				return DefaultComponentDescriptor.DeepCopy()
			},
			patchStrategicMerge: func(cfg *v1alpha1.Configuration, source client.Object, filename string) *v1alpha1.Configuration {
				cfg.Spec.PatchStrategicMerge = &v1alpha1.PatchStrategicMerge{
					Source: v1alpha1.PatchStrategicMergeSource{
						SourceRef: v1alpha1.PatchStrategicMergeSourceRef{
							Kind:      "GitRepository",
							Name:      source.GetName(),
							Namespace: source.GetNamespace(),
						},
						Path: filename,
					},
					Target: v1alpha1.PatchStrategicMergeTarget{
						Path: "merge-target/merge-target.yaml",
					},
				}
				cfg.Spec.ConfigRef = nil
				return cfg
			},
			snapshot: func(cv *v1alpha1.ComponentVersion, resource *v1alpha1.Resource) *v1alpha1.Snapshot {
				identity := v1alpha1.Identity{
					v1alpha1.ComponentNameKey:    cv.Spec.Component,
					v1alpha1.ComponentVersionKey: cv.Status.ReconciledVersion,
					v1alpha1.ResourceNameKey:     resource.Spec.Resource.Name,
					v1alpha1.ResourceVersionKey:  resource.Spec.Resource.Version,
				}
				sourceSnapshot := &v1alpha1.Snapshot{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-snapshot",
						Namespace: cv.Namespace,
					},
					Spec: v1alpha1.SnapshotSpec{
						Identity: identity,
					},
				}
				return sourceSnapshot
			},
			source: func(snapshot *v1alpha1.Snapshot) v1alpha1.Source {
				return v1alpha1.Source{
					SourceRef: &meta.NamespacedObjectKindReference{
						APIVersion: v1alpha1.GroupVersion.String(),
						Kind:       "Snapshot",
						Name:       snapshot.Name,
						Namespace:  snapshot.Namespace,
					},
				}
			},
			mock: func(fakeCache *cachefakes.FakeCache, fakeOcm *fakes.MockFetcher) {
				content, err := os.Open(filepath.Join("testdata", "merge-target.tar.gz"))
				require.NoError(t, err)
				fakeCache.FetchDataByDigestReturns(content, nil)
				fakeOcm.GetResourceReturnsOnCall(0, nil, nil)
				fakeOcm.GetResourceReturnsOnCall(1, nil, nil)
			},
		},
	}
	for i, tt := range testCases {
		t.Run(fmt.Sprintf("%d: %s", i, tt.name), func(t *testing.T) {
			cv := tt.componentVersion()
			resource := DefaultResource.DeepCopy()
			cd := tt.componentDescriptor()
			snapshot := tt.snapshot(cv, resource)
			source := tt.source(snapshot)
			configuration := DefaultConfiguration.DeepCopy()
			configuration.Spec.Source = source

			objs := []client.Object{cv, resource, cd, configuration}

			if snapshot != nil {
				objs = append(objs, snapshot)
			}

			if tt.patchStrategicMerge != nil {
				path := "/file.tar.gz"
				server := ghttp.NewServer()
				server.RouteToHandler("GET", path, func(writer http.ResponseWriter, request *http.Request) {
					http.ServeFile(writer, request, "testdata/patch-repo.tar.gz")
				})
				checksum := "2f49fe50940c8c5918102070fc963e670d89fa242f77958d32c295b396a6539e"
				gitRepo := createGitRepository("patch-repo", "default", server.URL()+path, checksum)
				configuration = tt.patchStrategicMerge(configuration, gitRepo, "sites/eu-west-1/deployment.yaml")
				objs = append(objs, gitRepo)
				sourcev1.AddToScheme(env.scheme)
			}

			client := env.FakeKubeClient(WithObjets(objs...))
			cache := &cachefakes.FakeCache{}
			fakeOcm := &fakes.MockFetcher{}
			tt.mock(cache, fakeOcm)

			cr := ConfigurationReconciler{
				Client:    client,
				Scheme:    env.scheme,
				OCMClient: fakeOcm,
				Cache:     cache,
			}

			_, err := cr.reconcile(context.Background(), configuration)
			if tt.expectError != "" {
				require.ErrorContains(t, err, tt.expectError)
			} else {
				require.NoError(t, err)
				t.Log("check if target snapshot has been created and cache was called")

				snapshotOutput := &v1alpha1.Snapshot{}
				err = client.Get(context.Background(), types.NamespacedName{
					Namespace: configuration.Namespace,
					Name:      configuration.Spec.SnapshotTemplate.Name,
				}, snapshotOutput)
				require.NoError(t, err)

				if tt.patchStrategicMerge != nil {
					t.Log("verifying that the strategic merge was performed")
					args := cache.PushDataCallingArgumentsOnCall(0)
					data := args[0].(string)
					sourceFile := extractFileFromTarGz(t, io.NopCloser(bytes.NewBuffer([]byte(data))), "merge-target.yaml")
					deployment := appsv1.Deployment{}
					err = yaml.Unmarshal(sourceFile, &deployment)
					assert.NoError(t, err)
					assert.Equal(t, int32(2), *deployment.Spec.Replicas, "has correct number of replicas")
					assert.Equal(t, 2, len(deployment.Spec.Template.Spec.Containers), "has correct number of containers")
					assert.Equal(t, corev1.PullPolicy("Always"), deployment.Spec.Template.Spec.Containers[0].ImagePullPolicy)
				} else {
					t.Log("extracting the passed in data and checking if the configuration worked")
					args := cache.PushDataCallingArgumentsOnCall(0)
					data, name, version := args[0], args[1], args[2]
					assert.Equal(t, "999", version)
					assert.Equal(t, "sha-1009814895297045910", name)
					assert.Contains(
						t,
						data.(string),
						"PODINFO_UI_COLOR: bittersweet\n  PODINFO_UI_MESSAGE: this is a new message\n",
						"the configuration data should have been applied",
					)
				}
			}
		})
	}
}

func createGitRepository(name, namespace, artifactURL, checksum string) *sourcev1.GitRepository {
	updatedTime := time.Now()
	return &sourcev1.GitRepository{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: sourcev1.GitRepositorySpec{
			URL: "https://github.com/" + namespace + "/" + name,
			Reference: &sourcev1.GitRepositoryRef{
				Branch: "master",
			},
			Interval:          metav1.Duration{Duration: time.Second * 30},
			GitImplementation: "go-git",
		},
		Status: sourcev1.GitRepositoryStatus{
			ObservedGeneration: int64(1),
			Conditions: []metav1.Condition{
				{
					Type:               "Ready",
					Status:             metav1.ConditionTrue,
					LastTransitionTime: metav1.Time{Time: updatedTime},
					Reason:             "GitOperationSucceed",
					Message:            "Fetched revision: master/b8e362c206e3d0cbb7ed22ced771a0056455a2fb",
				},
			},
			URL: artifactURL,
			Artifact: &sourcev1.Artifact{
				Path:           "gitrepository/flux-system/test-tf-controller/b8e362c206e3d0cbb7ed22ced771a0056455a2fb.tar.gz",
				URL:            artifactURL,
				Revision:       "master/b8e362c206e3d0cbb7ed22ced771a0056455a2fb",
				Checksum:       checksum,
				LastUpdateTime: metav1.Time{Time: updatedTime},
			},
		},
	}
}

func extractFileFromTarGz(t *testing.T, data io.Reader, filename string) []byte {
	tarReader := tar.NewReader(data)

	result := &bytes.Buffer{}

	for {
		header, err := tarReader.Next()

		if err == io.EOF {
			break
		}

		assert.NoError(t, err)

		if filepath.Base(header.Name) == filename {
			_, err := io.Copy(result, tarReader)
			assert.NoError(t, err)
			break
		}
	}

	return result.Bytes()
}