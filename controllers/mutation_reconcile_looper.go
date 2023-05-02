// SPDX-FileCopyrightText: 2022 SAP SE or an SAP affiliate company and Open Component Model contributors.
//
// SPDX-License-Identifier: Apache-2.0

package controllers

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/cuecontext"
	"github.com/containers/image/v5/pkg/compression"
	"github.com/fluxcd/pkg/apis/meta"
	"github.com/fluxcd/pkg/runtime/conditions"
	sourcev1 "github.com/fluxcd/source-controller/api/v1"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/mandelsoft/spiff/spiffing"
	"github.com/mandelsoft/vfs/pkg/osfs"
	"github.com/open-component-model/ocm-controller/pkg/snapshot"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/dynamic"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/open-component-model/ocm/pkg/contexts/ocm/accessmethods/localblob"
	"github.com/open-component-model/ocm/pkg/contexts/ocm/accessmethods/ociartifact"
	"github.com/open-component-model/ocm/pkg/contexts/ocm/accessmethods/ociblob"
	ocmmetav1 "github.com/open-component-model/ocm/pkg/contexts/ocm/compdesc/meta/v1"
	"github.com/open-component-model/ocm/pkg/contexts/ocm/utils/localize"
	ocmruntime "github.com/open-component-model/ocm/pkg/runtime"
	"github.com/open-component-model/ocm/pkg/spiff"
	"github.com/open-component-model/ocm/pkg/utils"

	"github.com/open-component-model/ocm-controller/api/v1alpha1"
	"github.com/open-component-model/ocm-controller/pkg/cache"
	"github.com/open-component-model/ocm-controller/pkg/component"
	"github.com/open-component-model/ocm-controller/pkg/configdata"
	"github.com/open-component-model/ocm-controller/pkg/ocm"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
)

// errTar defines an error that occurs when the resource is not a tar archive.
var errTar = errors.New("expected tarred directory content for configuration/localization resources, got plain text")

// MutationReconcileLooper holds dependencies required to reconcile a mutation object.
type MutationReconcileLooper struct {
	Scheme         *runtime.Scheme
	OCMClient      ocm.Contract
	Client         client.Client
	Cache          cache.Cache
	DynamicClient  dynamic.Interface
	SnapshotWriter snapshot.Writer
}

// ReconcileMutationObject reconciles mutation objects and writes a snapshot to the cache.
func (m *MutationReconcileLooper) ReconcileMutationObject(ctx context.Context, obj v1alpha1.MutationObject) error {
	mutationSpec := obj.GetSpec()

	sourceData, err := m.getData(ctx, &mutationSpec.SourceRef)
	if err != nil {
		return fmt.Errorf("failed to get data for source ref: %w", err)
	}

	sourceID, err := m.getIdentity(ctx, &mutationSpec.SourceRef)
	if err != nil {
		return fmt.Errorf("failed to get identity for source ref: %w", err)
	}

	obj.GetStatus().LatestSourceVersion = sourceID[v1alpha1.ComponentVersionKey]

	if len(sourceData) == 0 {
		return fmt.Errorf("source resource data cannot be empty")
	}

	var (
		snapshotID ocmmetav1.Identity
		sourceDir  string
	)

	if mutationSpec.ConfigRef != nil {
		configData, err := m.getData(ctx, mutationSpec.ConfigRef)
		if err != nil {
			return fmt.Errorf("failed to get data for config ref: %w", err)
		}

		snapshotID, err = m.getIdentity(ctx, mutationSpec.ConfigRef)
		if err != nil {
			return fmt.Errorf("failed to get identity for config ref: %w", err)
		}

		obj.GetStatus().LatestConfigVersion = snapshotID[v1alpha1.ComponentVersionKey]

		// if values are not nil then this is configuration
		if mutationSpec.Values != nil {
			sourceDir, err = m.configure(ctx, sourceData, configData, mutationSpec.Values)
			if err != nil {
				return fmt.Errorf("failed to configure resource: %w", err)
			}

		} else { // if values are nil then this is localization
			refPath := mutationSpec.ConfigRef.ResourceRef.ReferencePath
			cv, err := m.getComponentVersion(ctx, mutationSpec.ConfigRef)
			if err != nil {
				return fmt.Errorf("failed to get component version: %w", err)
			}

			cd, err := component.GetComponentDescriptor(ctx, m.Client, refPath, cv.Status.ComponentDescriptor)
			if err != nil {
				return fmt.Errorf("failed to get component descriptor from version: %w", err)
			}
			if cd == nil {
				return fmt.Errorf("couldn't find component descriptor for reference '%s' or any root components", refPath)
			}

			sourceDir, err = m.localize(ctx, cv, sourceData, configData)
			if err != nil {
				return fmt.Errorf("failed to localize resource: %w", err)
			}
		}
	}

	if mutationSpec.PatchStrategicMerge != nil {
		tmpDir, err := os.MkdirTemp("", "kustomization-")
		if err != nil {
			err = fmt.Errorf("tmp dir error: %w", err)
			return err
		}
		defer os.RemoveAll(tmpDir)

		gitSource, err := m.getSource(ctx, mutationSpec.PatchStrategicMerge.Source.SourceRef)
		if err != nil {
			return err
		}

		obj.GetStatus().LatestPatchSourceVersion = gitSource.GetArtifact().Revision

		sourcePath := mutationSpec.PatchStrategicMerge.Source.Path
		targetPath := mutationSpec.PatchStrategicMerge.Target.Path

		sourceDir, snapshotID, err = m.strategicMergePatch(ctx, gitSource, sourceData, tmpDir, sourcePath, targetPath)
		if err != nil {
			return err
		}
	}

	defer os.RemoveAll(sourceDir)

	_, err = m.SnapshotWriter.Write(ctx, obj, sourceDir, snapshotID)
	return err
}

func (m *MutationReconcileLooper) configure(ctx context.Context, data []byte, configObj []byte, configValues *apiextensionsv1.JSON) (string, error) {
	log := log.FromContext(ctx)

	virtualFS, err := osfs.NewTempFileSystem()
	if err != nil {
		return "", fmt.Errorf("fs error: %w", err)
	}

	fi, err := virtualFS.Stat("/")
	if err != nil {
		return "", fmt.Errorf("fs error: %w", err)
	}

	sourceDir := filepath.Join(os.TempDir(), fi.Name())

	if !isTar(data) {
		return "", errTar
	}

	if err := utils.ExtractTarToFs(virtualFS, bytes.NewBuffer(data)); err != nil {
		return "", fmt.Errorf("extract tar error: %w", err)
	}

	rules, err := m.createSubstitutionRulesForConfigurationValues(configObj, configValues)
	if err != nil {
		return "", err
	}

	if len(rules) == 0 {
		log.Info("no rules generated from the available config data; the generate snapshot will have no modifications")
	}

	if err := localize.Substitute(rules, virtualFS); err != nil {
		return "", fmt.Errorf("localization substitution failed: %w", err)
	}

	return sourceDir, nil
}

func (m *MutationReconcileLooper) localize(ctx context.Context, cv *v1alpha1.ComponentVersion, data, configObj []byte) (string, error) {
	log := log.FromContext(ctx)

	virtualFS, err := osfs.NewTempFileSystem()
	if err != nil {
		return "", fmt.Errorf("fs error: %w", err)
	}

	fi, err := virtualFS.Stat("/")
	if err != nil {
		return "", fmt.Errorf("fs error: %w", err)
	}

	sourceDir := filepath.Join(os.TempDir(), fi.Name())

	if !isTar(data) {
		return "", errTar
	}

	if err := utils.ExtractTarToFs(virtualFS, bytes.NewBuffer(data)); err != nil {
		return "", fmt.Errorf("extract tar error: %w", err)
	}

	rules, err := m.createSubstitutionRulesForLocalization(ctx, cv, configObj)
	if err != nil {
		return "", fmt.Errorf("failed to create substitution rules for localization: %w", err)
	}

	if len(rules) == 0 {
		log.Info("no rules generated from the available config data; the generate snapshot will have no modifications")
	}

	if err := localize.Substitute(rules, virtualFS); err != nil {
		return "", fmt.Errorf("localization substitution failed: %w", err)
	}

	return sourceDir, nil
}

func (m *MutationReconcileLooper) writeToCache(ctx context.Context, identity ocmmetav1.Identity, artifactPath string, version string) (string, error) {
	file, err := os.Open(artifactPath)
	if err != nil {
		return "", fmt.Errorf("failed to open created archive: %w", err)
	}
	defer file.Close()

	name, err := ocm.ConstructRepositoryName(identity)
	if err != nil {
		return "", fmt.Errorf("failed to construct name: %w", err)
	}

	digest, err := m.Cache.PushData(ctx, file, name, version)
	if err != nil {
		return "", fmt.Errorf("failed to push blob to local registry: %w", err)
	}

	return digest, nil
}

func (m *MutationReconcileLooper) fetchDataFromObjectReference(ctx context.Context, obj *v1alpha1.ObjectReference) ([]byte, error) {
	logger := log.FromContext(ctx)

	gvr := obj.GetGVR()
	src, err := m.DynamicClient.Resource(gvr).Namespace(obj.Namespace).Get(ctx, obj.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	snapshotName, ok, err := unstructured.NestedString(src.Object, "status", "snapshotName")
	if err != nil {
		return nil, fmt.Errorf("failed get the get snapshot: %w", err)
	}
	if !ok {
		return nil, errors.New("snapshot name not found in status")
	}

	key := types.NamespacedName{
		Name:      snapshotName,
		Namespace: obj.Namespace,
	}

	snapshot := &v1alpha1.Snapshot{}
	if err := m.Client.Get(ctx, key, snapshot); err != nil {
		if apierrors.IsNotFound(err) {
			logger.Info("snapshot doesn't exist", "snapshot", key)
			return nil, err
		}
		return nil,
			fmt.Errorf("failed to get component object: %w", err)
	}

	if conditions.IsFalse(snapshot, meta.ReadyCondition) {
		return nil, fmt.Errorf("snapshot not ready: %s", key)
	}

	snapshotData, err := m.getSnapshotBytes(ctx, snapshot)
	if err != nil {
		return nil, err
	}

	return snapshotData, nil
}

func (m *MutationReconcileLooper) fetchDataFromComponentVersion(ctx context.Context, obj *v1alpha1.ObjectReference) ([]byte, error) {
	key := types.NamespacedName{
		Name:      obj.Name,
		Namespace: obj.Namespace,
	}

	componentVersion := &v1alpha1.ComponentVersion{}
	if err := m.Client.Get(ctx, key, componentVersion); err != nil {
		return nil, err
	}

	octx, err := m.OCMClient.CreateAuthenticatedOCMContext(ctx, componentVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to create authenticated client: %w", err)
	}

	resource, _, err := m.OCMClient.GetResource(ctx, octx, componentVersion, obj.ResourceRef)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch resource from component version: %w", err)
	}
	defer resource.Close()

	uncompressed, _, err := compression.AutoDecompress(resource)
	if err != nil {
		return nil, fmt.Errorf("failed to auto decompress: %w", err)
	}
	defer uncompressed.Close()

	// This will be problematic with a 6 Gig large object when it's trying to read it all.
	content, err := io.ReadAll(uncompressed)
	if err != nil {
		return nil, fmt.Errorf("failed to read resource data: %w", err)
	}

	return content, nil
}

// This might be problematic if the resource is too large in the snapshot. ReadAll will read it into memory.
func (m *MutationReconcileLooper) getSnapshotBytes(ctx context.Context, snapshot *v1alpha1.Snapshot) ([]byte, error) {
	name, err := ocm.ConstructRepositoryName(snapshot.Spec.Identity)
	if err != nil {
		return nil, fmt.Errorf("failed to construct name: %w", err)
	}

	reader, err := m.Cache.FetchDataByDigest(ctx, name, snapshot.Status.LastReconciledDigest)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch data: %w", err)
	}

	uncompressed, _, err := compression.AutoDecompress(reader)
	if err != nil {
		return nil, fmt.Errorf("failed to auto decompress: %w", err)
	}
	defer uncompressed.Close()

	// We don't decompress snapshots because those are archives and are decompressed by the caching layer already.
	return io.ReadAll(uncompressed)
}

func (m *MutationReconcileLooper) createSubstitutionRulesForLocalization(ctx context.Context, cv *v1alpha1.ComponentVersion, data []byte) (localize.Substitutions, error) {
	config := &configdata.ConfigData{}
	if err := ocmruntime.DefaultYAMLEncoding.Unmarshal(data, config); err != nil {
		return nil,
			fmt.Errorf("failed to unmarshal content: %w", err)
	}

	octx, err := m.OCMClient.CreateAuthenticatedOCMContext(ctx, cv)
	if err != nil {
		return nil, fmt.Errorf("failed to create authenticated client: %w", err)
	}

	compvers, err := m.OCMClient.GetComponentVersion(ctx, octx, cv, cv.Spec.Component, cv.Status.ReconciledVersion)
	if err != nil {
		return nil, fmt.Errorf("failed to get component version: %w", err)
	}
	defer compvers.Close()

	var localizations localize.Substitutions
	for _, l := range config.Localization {
		if l.Mapping != nil {
			res, err := m.compileMapping(ctx, cv, l.Mapping.Transform)
			if err != nil {
				return nil, fmt.Errorf("failed to compile mapping: %w", err)
			}
			if err := localizations.Add("custom", l.File, l.Mapping.Path, res); err != nil {
				return nil, fmt.Errorf("failed to add identifier: %w", err)
			}
			continue
		}

		resource, err := compvers.GetResource(ocmmetav1.NewIdentity(l.Resource.Name))
		if err != nil {
			return nil, fmt.Errorf("failed to fetch resource from component version: %w", err)
		}

		accSpec, err := resource.Access()
		if err != nil {
			return nil, err
		}

		var (
			ref    string
			refErr error
		)

		for ref == "" && refErr == nil {
			switch x := accSpec.(type) {
			case *ociartifact.AccessSpec:
				ref = x.ImageReference
			case *ociblob.AccessSpec:
				ref = fmt.Sprintf("%s@%s", x.Reference, x.Digest)
			case *localblob.AccessSpec:
				if x.GlobalAccess == nil {
					refErr = errors.New("cannot determine image digest")
				}
				accSpec, refErr = octx.AccessSpecForSpec(x.GlobalAccess)
			default:
				refErr = errors.New("cannot determine access spec type")
			}
		}

		if refErr != nil {
			return nil, fmt.Errorf("failed to parse access reference: %w", refErr)
		}

		pRef, err := name.ParseReference(ref)
		if err != nil {
			return nil, fmt.Errorf("failed to parse access reference: %w", err)
		}

		if l.Registry != "" {
			if err := localizations.Add("registry", l.File, l.Registry, pRef.Context().Registry.Name()); err != nil {
				return nil, fmt.Errorf("failed to add registry: %w", err)
			}
		}

		if l.Repository != "" {
			if err := localizations.Add("repository", l.File, l.Repository, pRef.Context().RepositoryStr()); err != nil {
				return nil, fmt.Errorf("failed to add repository: %w", err)
			}
		}

		if l.Image != "" {
			if err := localizations.Add("image", l.File, l.Image, pRef.Name()); err != nil {
				return nil, fmt.Errorf("failed to add image ref name: %w", err)
			}
		}

		if l.Tag != "" {
			if err := localizations.Add("tag", l.File, l.Tag, pRef.Identifier()); err != nil {
				return nil, fmt.Errorf("failed to add identifier: %w", err)
			}
		}
	}

	return localizations, nil
}

func (m *MutationReconcileLooper) createSubstitutionRulesForConfigurationValues(data []byte, values *apiextensionsv1.JSON) (localize.Substitutions, error) {
	config := &configdata.ConfigData{}
	if err := ocmruntime.DefaultYAMLEncoding.Unmarshal(data, config); err != nil {
		return nil,
			fmt.Errorf("failed to unmarshal content: %w", err)
	}

	var rules localize.Substitutions
	for i, l := range config.Configuration.Rules {
		if err := rules.Add(fmt.Sprintf("subst-%d", i), l.File, l.Path, l.Value); err != nil {
			return nil, fmt.Errorf("failed to add rule: %w", err)
		}
	}

	defaults, err := json.Marshal(config.Configuration.Defaults)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal configuration defaults: %w", err) //nolint:staticcheck // it's fine
	}

	schema, err := json.Marshal(config.Configuration.Schema) //nolint:staticcheck // it's fine
	if err != nil {
		return nil, fmt.Errorf("failed to marshal configuration schema: %w", err)
	}

	configSubstitutions, err := m.configurator(rules, defaults, values.Raw, schema)
	if err != nil {
		return nil, fmt.Errorf("configurator error: %w", err)
	}

	return configSubstitutions, nil
}

func (m *MutationReconcileLooper) configurator(subst []localize.Substitution, defaults, values, schema []byte) (localize.Substitutions, error) {
	// configure defaults
	templ := make(map[string]any)
	if err := ocmruntime.DefaultYAMLEncoding.Unmarshal(defaults, &templ); err != nil {
		return nil, fmt.Errorf("cannot unmarshal template: %w", err)
	}

	// configure values overrides... must be a better way
	var valuesMap map[string]any
	if err := ocmruntime.DefaultYAMLEncoding.Unmarshal(values, &valuesMap); err != nil {
		return nil, fmt.Errorf("cannot unmarshal values: %w", err)
	}

	for k, v := range valuesMap {
		if _, ok := templ[k]; ok {
			templ[k] = v
		}
	}

	// configure adjustments
	list := []any{}
	for _, e := range subst {
		list = append(list, e)
	}

	templ["adjustments"] = list

	templateBytes, err := ocmruntime.DefaultJSONEncoding.Marshal(templ)
	if err != nil {
		return nil, fmt.Errorf("cannot marshal template: %w", err)
	}

	if len(schema) > 0 {
		if err := spiff.ValidateByScheme(values, schema); err != nil {
			return nil, fmt.Errorf("validation failed: %w", err)
		}
	}

	config, err := spiff.CascadeWith(spiff.TemplateData("adjustments", templateBytes), spiff.Mode(spiffing.MODE_PRIVATE))
	if err != nil {
		return nil, fmt.Errorf("error while doing cascade with: %w", err)
	}

	var result struct {
		Adjustments localize.Substitutions `json:"adjustments,omitempty"`
	}

	if err := ocmruntime.DefaultYAMLEncoding.Unmarshal(config, &result); err != nil {
		return nil, fmt.Errorf("error unmarshaling result: %w", err)
	}

	return result.Adjustments, nil
}

func (m *MutationReconcileLooper) compileMapping(ctx context.Context, cv *v1alpha1.ComponentVersion, mapping string) (json.RawMessage, error) {
	cueCtx := cuecontext.New()
	cd, err := component.GetComponentDescriptor(ctx, m.Client, nil, cv.Status.ComponentDescriptor)
	if err != nil {
		return nil, err
	}

	if cd == nil {
		return nil, fmt.Errorf("component descriptor not found with ref: %+v", cv.Status.ComponentDescriptor.ComponentDescriptorRef)
	}

	// first create the component descriptor struct
	root := cueCtx.CompileString("component:{}").FillPath(cue.ParsePath("component"), cueCtx.Encode(cd.Spec))

	// populate with refs
	root, err = m.populateReferences(ctx, root, cv.GetNamespace())
	if err != nil {
		return nil, err
	}

	// populate the mapping
	v := cueCtx.CompileString(mapping, cue.Scope(root))

	// resolve the output
	res, err := v.LookupPath(cue.ParsePath("out")).Bytes()
	if err != nil {
		return nil, err
	}

	// format the result
	var out json.RawMessage
	if err := json.Unmarshal(res, &out); err != nil {
		return nil, err
	}

	return out, nil
}

func (m *MutationReconcileLooper) populateReferences(ctx context.Context, src cue.Value, namespace string) (cue.Value, error) {
	root := src

	path := cue.ParsePath("component.references")

	refs := root.LookupPath(path)
	if !refs.Exists() {
		return src, nil
	}

	refList, err := refs.List()
	if err != nil {
		return src, err
	}

	for refList.Next() {
		val := refList.Value()
		index := refList.Selector()

		refData, err := val.Struct()
		if err != nil {
			return src, err
		}

		refName, err := getStructFieldValue(refData, "componentName")
		if err != nil {
			return src, err
		}

		refVersion, err := getStructFieldValue(refData, "version")
		if err != nil {
			return src, err
		}

		refCDRef, err := component.ConstructUniqueName(refName, refVersion, ocmmetav1.Identity{})
		if err != nil {
			return src, err
		}

		ref := v1alpha1.Reference{
			Name:    refName,
			Version: refVersion,
			ComponentDescriptorRef: meta.NamespacedObjectReference{
				Namespace: namespace,
				Name:      refCDRef,
			},
		}

		cd, err := component.GetComponentDescriptor(ctx, m.Client, nil, ref)
		if err != nil {
			return src, err
		}

		val = val.FillPath(cue.ParsePath("component"), cd.Spec)

		val, err = m.populateReferences(ctx, val, namespace)
		if err != nil {
			return src, err
		}

		root = root.FillPath(cue.MakePath(cue.Str("component"), cue.Str("references"), index), val)
	}

	return root, nil
}

func getStructFieldValue(v *cue.Struct, field string) (string, error) {
	f, err := v.FieldByName(field, false)
	if err != nil {
		return "", err
	}
	return f.Value.String()
}

func (m *MutationReconcileLooper) getSource(ctx context.Context, ref meta.NamespacedObjectKindReference) (sourcev1.Source, error) {
	var obj client.Object
	switch ref.Kind {
	case sourcev1.GitRepositoryKind:
		obj = &sourcev1.GitRepository{}
	//TODO: these are not part of source-controller v1 yet, consider renabling when they are
	// case sourcev1.BucketKind:
	//     obj = &sourcev1.Bucket{}
	// case sourcev1.OCIRepositoryKind:
	//     obj = &sourcev1.OCIRepository{}
	default:
		return nil, fmt.Errorf("source `%s` kind '%s' not supported", ref.Name, ref.Kind)
	}

	key := types.NamespacedName{
		Name:      ref.Name,
		Namespace: ref.Namespace,
	}

	err := m.Client.Get(ctx, key, obj)
	if err != nil {
		return nil, fmt.Errorf("unable to get source '%s': %w", key, err)
	}

	return obj.(sourcev1.Source), nil
}

func (m *MutationReconcileLooper) getData(ctx context.Context, obj *v1alpha1.ObjectReference) ([]byte, error) {
	var (
		data []byte
		err  error
	)

	switch obj.Kind {
	case v1alpha1.ComponentVersionKind:
		if data, err = m.fetchDataFromComponentVersion(ctx, obj); err != nil {
			return nil,
				fmt.Errorf("failed to fetch resource data from resource ref: %w", err)
		}
	default:
		if data, err = m.fetchDataFromObjectReference(ctx, obj); err != nil {
			return nil,
				fmt.Errorf("failed to fetch resource data from snapshot: %w", err)
		}
	}

	return data, err
}

func (m *MutationReconcileLooper) getIdentity(ctx context.Context, obj *v1alpha1.ObjectReference) (ocmmetav1.Identity, error) {
	var (
		id  ocmmetav1.Identity
		err error
	)

	key := types.NamespacedName{
		Name:      obj.Name,
		Namespace: obj.Namespace,
	}

	switch obj.Kind {
	case v1alpha1.ComponentVersionKind:
		cv := &v1alpha1.ComponentVersion{}
		if err := m.Client.Get(ctx, key, cv); err != nil {
			return nil, err
		}

		id = ocmmetav1.Identity{
			v1alpha1.ComponentNameKey:    cv.Status.ComponentDescriptor.ComponentDescriptorRef.Name,
			v1alpha1.ComponentVersionKey: cv.Status.ComponentDescriptor.Version,
			v1alpha1.ResourceNameKey:     obj.ResourceRef.Name,
			v1alpha1.ResourceVersionKey:  obj.ResourceRef.Version,
		}
	default:
		// if kind is not ComponentVersion, then fetch resource using dynamic client
		// and get the snapshot name from the resource
		gvr := obj.GetGVR()
		src, err := m.DynamicClient.Resource(gvr).Namespace(obj.Namespace).Get(ctx, obj.Name, metav1.GetOptions{})
		if err != nil {
			return nil, err
		}

		snapshotName, ok, err := unstructured.NestedString(src.Object, "status", "snapshotName")
		if err != nil {
			return nil, fmt.Errorf("failed get the get snapshot: %w", err)
		}
		if !ok {
			return nil, errors.New("snapshot name not found in status")
		}

		snapshot := &v1alpha1.Snapshot{}
		if err := m.Client.Get(ctx, types.NamespacedName{Namespace: obj.Namespace, Name: snapshotName}, snapshot); err != nil {
			return nil, err
		}

		id = snapshot.Spec.Identity
	}

	return id, err
}

func (m *MutationReconcileLooper) getComponentVersion(ctx context.Context, obj *v1alpha1.ObjectReference) (*v1alpha1.ComponentVersion, error) {
	if obj.Kind != v1alpha1.ComponentVersionKind {
		return nil, errors.New("cannot retrieve component version for snapshot")
	}

	key := types.NamespacedName{
		Name:      obj.Name,
		Namespace: obj.Namespace,
	}
	cv := &v1alpha1.ComponentVersion{}
	if err := m.Client.Get(ctx, key, cv); err != nil {
		return nil, err
	}
	return cv, nil
}
