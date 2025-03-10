/*
Copyright 2021 The KubeVela Authors.

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

package definition

import (
	"context"
	"encoding/json"
	"fmt"

	"cuelang.org/go/cue"
	"cuelang.org/go/cue/build"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	mycue "github.com/oam-dev/kubevela/pkg/cue"
	"github.com/oam-dev/kubevela/pkg/dsl/model"
	"github.com/oam-dev/kubevela/pkg/dsl/process"
	"github.com/oam-dev/kubevela/pkg/dsl/task"
	"github.com/oam-dev/kubevela/pkg/oam"
	"github.com/oam-dev/kubevela/pkg/oam/util"
)

const (
	// OutputFieldName is the name of the struct contains the CR data
	OutputFieldName = process.OutputFieldName
	// OutputsFieldName is the name of the struct contains the map[string]CR data
	OutputsFieldName = process.OutputsFieldName
	// PatchFieldName is the name of the struct contains the patch of CR data
	PatchFieldName = "patch"
	// CustomMessage defines the custom message in definition template
	CustomMessage = "message"
	// HealthCheckPolicy defines the health check policy in definition template
	HealthCheckPolicy = "isHealth"
)

const (
	// AuxiliaryWorkload defines the extra workload obj from a workloadDefinition,
	// e.g. a workload composed by deployment and service, the service will be marked as AuxiliaryWorkload
	AuxiliaryWorkload = "AuxiliaryWorkload"
)

// AbstractEngine defines Definition's Render interface
type AbstractEngine interface {
	Complete(ctx process.Context, abstractTemplate string, params interface{}) error
	HealthCheck(ctx process.Context, cli client.Client, ns string, healthPolicyTemplate string) (bool, error)
	Status(ctx process.Context, cli client.Client, ns string, customStatusTemplate string) (string, error)
}

type def struct {
	name string
	pd   *PackageDiscover
}

type workloadDef struct {
	def
}

// NewWorkloadAbstractEngine create Workload Definition AbstractEngine
func NewWorkloadAbstractEngine(name string, pd *PackageDiscover) AbstractEngine {
	return &workloadDef{
		def: def{
			name: name,
			pd:   pd,
		},
	}
}

// Complete do workload definition's rendering
func (wd *workloadDef) Complete(ctx process.Context, abstractTemplate string, params interface{}) error {
	bi := build.NewContext().NewInstance("", nil)
	if err := bi.AddFile("-", abstractTemplate); err != nil {
		return errors.WithMessagef(err, "invalid cue template of workload %s", wd.name)
	}
	var paramFile = "parameter: {}"
	if params != nil {
		bt, err := json.Marshal(params)
		if err != nil {
			return errors.WithMessagef(err, "marshal parameter of workload %s", wd.name)
		}
		if string(bt) != "null" {
			paramFile = fmt.Sprintf("%s: %s", mycue.ParameterTag, string(bt))
		}
	}
	if err := bi.AddFile("parameter", paramFile); err != nil {
		return errors.WithMessagef(err, "invalid parameter of workload %s", wd.name)
	}

	if err := bi.AddFile("-", ctx.ExtendedContextFile()); err != nil {
		return err
	}

	inst, err := wd.pd.ImportPackagesAndBuildInstance(bi)
	if err != nil {
		return err
	}

	if err := inst.Value().Validate(); err != nil {
		return errors.WithMessagef(err, "invalid cue template of workload %s after merge parameter and context", wd.name)
	}
	output := inst.Lookup(OutputFieldName)
	base, err := model.NewBase(output)
	if err != nil {
		return errors.WithMessagef(err, "invalid output of workload %s", wd.name)
	}
	if err := ctx.SetBase(base); err != nil {
		return err
	}

	// we will support outputs for workload composition, and it will become trait in AppConfig.
	outputs := inst.Lookup(OutputsFieldName)
	if !outputs.Exists() {
		return nil
	}
	st, err := outputs.Struct()
	if err != nil {
		return errors.WithMessagef(err, "invalid outputs of workload %s", wd.name)
	}
	for i := 0; i < st.Len(); i++ {
		fieldInfo := st.Field(i)
		if fieldInfo.IsDefinition || fieldInfo.IsHidden || fieldInfo.IsOptional {
			continue
		}
		other, err := model.NewOther(fieldInfo.Value)
		if err != nil {
			return errors.WithMessagef(err, "invalid outputs(%s) of workload %s", fieldInfo.Name, wd.name)
		}
		if err := ctx.AppendAuxiliaries(process.Auxiliary{Ins: other, Type: AuxiliaryWorkload, Name: fieldInfo.Name}); err != nil {
			return err
		}
	}
	return nil
}

func (wd *workloadDef) getTemplateContext(ctx process.Context, cli client.Reader, ns string) (map[string]interface{}, error) {

	var root = initRoot(ctx.BaseContextLabels())
	var commonLabels = GetCommonLabels(ctx.BaseContextLabels())

	base, assists := ctx.Output()
	componentWorkload, err := base.Unstructured()
	if err != nil {
		return nil, err
	}
	// workload main resource will have a unique label("app.oam.dev/resourceType"="WORKLOAD") in per component/app level
	object, err := getResourceFromObj(componentWorkload, cli, ns, util.MergeMapOverrideWithDst(map[string]string{
		oam.LabelOAMResourceType: oam.ResourceTypeWorkload,
	}, commonLabels), "")
	if err != nil {
		return nil, err
	}
	root[OutputFieldName] = object
	outputs := make(map[string]interface{})
	for _, assist := range assists {
		if assist.Type != AuxiliaryWorkload {
			continue
		}
		if assist.Name == "" {
			return nil, errors.New("the auxiliary of workload must have a name with format 'outputs.<my-name>'")
		}
		traitRef, err := assist.Ins.Unstructured()
		if err != nil {
			return nil, err
		}
		// AuxiliaryWorkload will have a unique label("trait.oam.dev/resource"="name of outputs") in per component/app level
		object, err := getResourceFromObj(traitRef, cli, ns, util.MergeMapOverrideWithDst(map[string]string{
			oam.TraitTypeLabel: AuxiliaryWorkload,
		}, commonLabels), assist.Name)
		if err != nil {
			return nil, err
		}
		outputs[assist.Name] = object
	}
	if len(outputs) > 0 {
		root[OutputsFieldName] = outputs
	}
	return root, nil
}

// HealthCheck address health check for workload
func (wd *workloadDef) HealthCheck(ctx process.Context, cli client.Client, ns string, healthPolicyTemplate string) (bool, error) {
	if healthPolicyTemplate == "" {
		return true, nil
	}
	templateContext, err := wd.getTemplateContext(ctx, cli, ns)
	if err != nil {
		return false, errors.WithMessage(err, "get template context")
	}
	return checkHealth(templateContext, healthPolicyTemplate)
}

func checkHealth(templateContext map[string]interface{}, healthPolicyTemplate string) (bool, error) {
	bt, err := json.Marshal(templateContext)
	if err != nil {
		return false, errors.WithMessage(err, "json marshal template context")
	}

	var buff = "context: " + string(bt) + "\n" + healthPolicyTemplate
	var r cue.Runtime
	inst, err := r.Compile("-", buff)
	if err != nil {
		return false, errors.WithMessage(err, "compile health template")
	}
	healthy, err := inst.Lookup(HealthCheckPolicy).Bool()
	if err != nil {
		return false, errors.WithMessage(err, "evaluate health status")
	}
	return healthy, nil
}

// Status get workload status by customStatusTemplate
func (wd *workloadDef) Status(ctx process.Context, cli client.Client, ns string, customStatusTemplate string) (string, error) {
	if customStatusTemplate == "" {
		return "", nil
	}
	templateContext, err := wd.getTemplateContext(ctx, cli, ns)
	if err != nil {
		return "", errors.WithMessage(err, "get template context")
	}
	return getStatusMessage(templateContext, customStatusTemplate)
}

func getStatusMessage(templateContext map[string]interface{}, customStatusTemplate string) (string, error) {
	bt, err := json.Marshal(templateContext)
	if err != nil {
		return "", errors.WithMessage(err, "json marshal template context")
	}
	var buff = "context: " + string(bt) + "\n" + customStatusTemplate
	var r cue.Runtime
	inst, err := r.Compile("-", buff)
	if err != nil {
		return "", errors.WithMessage(err, "compile customStatus template")
	}
	message, err := inst.Lookup(CustomMessage).String()
	if err != nil {
		return "", errors.WithMessage(err, "evaluate customStatus.message")
	}
	return message, nil
}

type traitDef struct {
	def
}

// NewTraitAbstractEngine create Trait Definition AbstractEngine
func NewTraitAbstractEngine(name string, pd *PackageDiscover) AbstractEngine {
	return &traitDef{
		def: def{
			name: name,
			pd:   pd,
		},
	}
}

// Complete do trait definition's rendering
func (td *traitDef) Complete(ctx process.Context, abstractTemplate string, params interface{}) error {
	bi := build.NewContext().NewInstance("", nil)
	if err := bi.AddFile("-", abstractTemplate); err != nil {
		return errors.WithMessagef(err, "invalid template of trait %s", td.name)
	}
	var paramFile = "parameter: {}"
	if params != nil {
		bt, err := json.Marshal(params)
		if err != nil {
			return errors.WithMessagef(err, "marshal parameter of trait %s", td.name)
		}
		if string(bt) != "null" {
			paramFile = fmt.Sprintf("%s: %s", mycue.ParameterTag, string(bt))
		}
	}
	if err := bi.AddFile("parameter", paramFile); err != nil {
		return errors.WithMessagef(err, "invalid parameter of trait %s", td.name)
	}
	if err := bi.AddFile("context", ctx.ExtendedContextFile()); err != nil {
		return errors.WithMessagef(err, "invalid context of trait %s", td.name)
	}

	inst, err := td.pd.ImportPackagesAndBuildInstance(bi)
	if err != nil {
		return err
	}

	if err := inst.Value().Validate(); err != nil {
		return errors.WithMessagef(err, "invalid template of trait %s after merge with parameter and context", td.name)
	}
	processing := inst.Lookup("processing")
	if processing.Exists() {
		if inst, err = task.Process(inst); err != nil {
			return errors.WithMessagef(err, "invalid process of trait %s", td.name)
		}
	}
	outputs := inst.Lookup(OutputsFieldName)
	if outputs.Exists() {
		st, err := outputs.Struct()
		if err != nil {
			return errors.WithMessagef(err, "invalid outputs of trait %s", td.name)
		}
		for i := 0; i < st.Len(); i++ {
			fieldInfo := st.Field(i)
			if fieldInfo.IsDefinition || fieldInfo.IsHidden || fieldInfo.IsOptional {
				continue
			}
			other, err := model.NewOther(fieldInfo.Value)
			if err != nil {
				return errors.WithMessagef(err, "invalid outputs(resource=%s) of trait %s", fieldInfo.Name, td.name)
			}
			if err := ctx.AppendAuxiliaries(process.Auxiliary{Ins: other, Type: td.name, Name: fieldInfo.Name}); err != nil {
				return err
			}
		}
	}

	patcher := inst.Lookup(PatchFieldName)
	if patcher.Exists() {
		base, _ := ctx.Output()
		p, err := model.NewOther(patcher)
		if err != nil {
			return errors.WithMessagef(err, "invalid patch of trait %s", td.name)
		}
		if err := base.Unify(p); err != nil {
			return errors.WithMessagef(err, "invalid patch trait %s into workload", td.name)
		}
	}

	return nil
}

// GetCommonLabels will convert context based labels to OAM standard labels
func GetCommonLabels(contextLabels map[string]string) map[string]string {
	var commonLabels = map[string]string{}
	for k, v := range contextLabels {
		switch k {
		case process.ContextAppName:
			commonLabels[oam.LabelAppName] = v
		case process.ContextName:
			commonLabels[oam.LabelAppComponent] = v
		case process.ContextAppRevision:
			commonLabels[oam.LabelAppRevision] = v
		}
	}
	return commonLabels
}

func initRoot(contextLabels map[string]string) map[string]interface{} {
	var root = map[string]interface{}{}
	for k, v := range contextLabels {
		root[k] = v
	}
	return root
}

func (td *traitDef) getTemplateContext(ctx process.Context, cli client.Reader, ns string) (map[string]interface{}, error) {
	var root = initRoot(ctx.BaseContextLabels())
	var commonLabels = GetCommonLabels(ctx.BaseContextLabels())

	_, assists := ctx.Output()
	outputs := make(map[string]interface{})
	for _, assist := range assists {
		if assist.Type != td.name {
			continue
		}
		traitRef, err := assist.Ins.Unstructured()
		if err != nil {
			return nil, err
		}
		object, err := getResourceFromObj(traitRef, cli, ns, util.MergeMapOverrideWithDst(map[string]string{
			oam.TraitTypeLabel: assist.Type,
		}, commonLabels), assist.Name)
		if err != nil {
			return nil, err
		}
		outputs[assist.Name] = object
	}
	if len(outputs) > 0 {
		root[OutputsFieldName] = outputs
	}
	return root, nil
}

// Status get trait status by customStatusTemplate
func (td *traitDef) Status(ctx process.Context, cli client.Client, ns string, customStatusTemplate string) (string, error) {
	if customStatusTemplate == "" {
		return "", nil
	}
	templateContext, err := td.getTemplateContext(ctx, cli, ns)
	if err != nil {
		return "", errors.WithMessage(err, "get template context")
	}
	return getStatusMessage(templateContext, customStatusTemplate)
}

// HealthCheck address health check for trait
func (td *traitDef) HealthCheck(ctx process.Context, cli client.Client, ns string, healthPolicyTemplate string) (bool, error) {
	if healthPolicyTemplate == "" {
		return true, nil
	}
	templateContext, err := td.getTemplateContext(ctx, cli, ns)
	if err != nil {
		return false, errors.WithMessage(err, "get template context")
	}
	return checkHealth(templateContext, healthPolicyTemplate)
}

func getResourceFromObj(obj *unstructured.Unstructured, client client.Reader, namespace string, labels map[string]string, outputsResource string) (map[string]interface{}, error) {
	if outputsResource != "" {
		labels[oam.TraitResource] = outputsResource
	}
	if obj.GetName() != "" {
		u, err := util.GetObjectGivenGVKAndName(context.Background(), client, obj.GroupVersionKind(), namespace, obj.GetName())
		if err != nil {
			return nil, err
		}
		return u.Object, nil
	}
	list, err := util.GetObjectsGivenGVKAndLabels(context.Background(), client, obj.GroupVersionKind(), namespace, labels)
	if err != nil {
		return nil, err
	}
	if len(list.Items) == 1 {
		return list.Items[0].Object, nil
	}
	for _, v := range list.Items {
		if v.GetLabels()[oam.TraitResource] == outputsResource {
			return v.Object, nil
		}
	}
	return nil, errors.Errorf("no resources found gvk(%v) labels(%v)", obj.GroupVersionKind(), labels)
}
