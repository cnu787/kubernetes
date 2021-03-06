/*
Copyright 2015 The Kubernetes Authors.

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

package cmd

import (
	"fmt"
	"io"

	utilerrors "k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/kubernetes/pkg/kubectl"
	"k8s.io/kubernetes/pkg/kubectl/cmd/templates"
	cmdutil "k8s.io/kubernetes/pkg/kubectl/cmd/util"
	"k8s.io/kubernetes/pkg/kubectl/genericclioptions"
	"k8s.io/kubernetes/pkg/kubectl/resource"
	"k8s.io/kubernetes/pkg/kubectl/util/i18n"

	"github.com/golang/glog"
	"github.com/spf13/cobra"
)

var (
	autoscaleLong = templates.LongDesc(i18n.T(`
		Creates an autoscaler that automatically chooses and sets the number of pods that run in a kubernetes cluster.

		Looks up a Deployment, ReplicaSet, or ReplicationController by name and creates an autoscaler that uses the given resource as a reference.
		An autoscaler can automatically increase or decrease number of pods deployed within the system as needed.`))

	autoscaleExample = templates.Examples(i18n.T(`
		# Auto scale a deployment "foo", with the number of pods between 2 and 10, no target CPU utilization specified so a default autoscaling policy will be used:
		kubectl autoscale deployment foo --min=2 --max=10

		# Auto scale a replication controller "foo", with the number of pods between 1 and 5, target CPU utilization at 80%:
		kubectl autoscale rc foo --max=5 --cpu-percent=80`))
)

type AutoscaleOptions struct {
	FilenameOptions resource.FilenameOptions
	RecordFlags     *genericclioptions.RecordFlags

	Recorder genericclioptions.Recorder
}

func NewAutoscaleOptions() *AutoscaleOptions {
	return &AutoscaleOptions{
		FilenameOptions: resource.FilenameOptions{},
		RecordFlags:     genericclioptions.NewRecordFlags(),
		Recorder:        genericclioptions.NoopRecorder{},
	}
}

func NewCmdAutoscale(f cmdutil.Factory, out io.Writer) *cobra.Command {
	o := NewAutoscaleOptions()

	validArgs := []string{"deployment", "replicaset", "replicationcontroller"}
	argAliases := kubectl.ResourceAliases(validArgs)

	cmd := &cobra.Command{
		Use: "autoscale (-f FILENAME | TYPE NAME | TYPE/NAME) [--min=MINPODS] --max=MAXPODS [--cpu-percent=CPU]",
		DisableFlagsInUseLine: true,
		Short:   i18n.T("Auto-scale a Deployment, ReplicaSet, or ReplicationController"),
		Long:    autoscaleLong,
		Example: autoscaleExample,
		Run: func(cmd *cobra.Command, args []string) {
			cmdutil.CheckErr(o.Complete(f, cmd))
			cmdutil.CheckErr(o.RunAutoscale(f, out, cmd, args))
		},
		ValidArgs:  validArgs,
		ArgAliases: argAliases,
	}

	// bind flag structs
	o.RecordFlags.AddFlags(cmd)

	cmdutil.AddPrinterFlags(cmd)
	cmd.Flags().String("generator", cmdutil.HorizontalPodAutoscalerV1GeneratorName, i18n.T("The name of the API generator to use. Currently there is only 1 generator."))
	cmd.Flags().Int32("min", -1, "The lower limit for the number of pods that can be set by the autoscaler. If it's not specified or negative, the server will apply a default value.")
	cmd.Flags().Int32("max", -1, "The upper limit for the number of pods that can be set by the autoscaler. Required.")
	cmd.MarkFlagRequired("max")
	cmd.Flags().Int32("cpu-percent", -1, fmt.Sprintf("The target average CPU utilization (represented as a percent of requested CPU) over all the pods. If it's not specified or negative, a default autoscaling policy will be used."))
	cmd.Flags().String("name", "", i18n.T("The name for the newly created object. If not specified, the name of the input resource will be used."))
	cmdutil.AddDryRunFlag(cmd)
	usage := "identifying the resource to autoscale."
	cmdutil.AddFilenameOptionFlags(cmd, &o.FilenameOptions, usage)
	cmdutil.AddApplyAnnotationFlags(cmd)
	return cmd
}

func (o *AutoscaleOptions) Complete(f cmdutil.Factory, cmd *cobra.Command) error {
	var err error

	o.RecordFlags.Complete(f.Command(cmd, false))
	o.Recorder, err = o.RecordFlags.ToRecorder()
	if err != nil {
		return err
	}

	return nil
}

func (o *AutoscaleOptions) RunAutoscale(f cmdutil.Factory, out io.Writer, cmd *cobra.Command, args []string) error {
	namespace, enforceNamespace, err := f.DefaultNamespace()
	if err != nil {
		return err
	}

	// validate flags
	if err := validateFlags(cmd); err != nil {
		return err
	}

	r := f.NewBuilder().
		Internal().
		ContinueOnError().
		NamespaceParam(namespace).DefaultNamespace().
		FilenameParam(enforceNamespace, &o.FilenameOptions).
		ResourceTypeOrNameArgs(false, args...).
		Flatten().
		Do()
	err = r.Err()
	if err != nil {
		return err
	}

	count := 0
	err = r.Visit(func(info *resource.Info, err error) error {
		if err != nil {
			return err
		}

		mapping := info.ResourceMapping()
		if err := f.CanBeAutoscaled(mapping.GroupVersionKind.GroupKind()); err != nil {
			return err
		}

		// get the generator
		var generator kubectl.StructuredGenerator
		switch generatorName := cmdutil.GetFlagString(cmd, "generator"); generatorName {
		case cmdutil.HorizontalPodAutoscalerV1GeneratorName:
			generator = &kubectl.HorizontalPodAutoscalerGeneratorV1{
				Name:               info.Name,
				MinReplicas:        cmdutil.GetFlagInt32(cmd, "min"),
				MaxReplicas:        cmdutil.GetFlagInt32(cmd, "max"),
				CPUPercent:         cmdutil.GetFlagInt32(cmd, "cpu-percent"),
				ScaleRefName:       info.Name,
				ScaleRefKind:       mapping.GroupVersionKind.Kind,
				ScaleRefApiVersion: mapping.GroupVersionKind.GroupVersion().String(),
			}
		default:
			return cmdutil.UsageErrorf(cmd, "Generator %s not supported. ", generatorName)
		}

		// Generate new object
		object, err := generator.StructuredGenerate()
		if err != nil {
			return err
		}

		mapper, typer := f.Object()
		resourceMapper := &resource.Mapper{
			ObjectTyper:  typer,
			RESTMapper:   mapper,
			ClientMapper: resource.ClientMapperFunc(f.ClientForMapping),
			Decoder:      cmdutil.InternalVersionDecoder(),
		}
		hpa, err := resourceMapper.InfoForObject(object, nil)
		if err != nil {
			return err
		}
		if err := o.Recorder.Record(hpa.Object); err != nil {
			glog.V(4).Infof("error recording current command: %v", err)
		}
		if cmdutil.GetDryRunFlag(cmd) {
			return cmdutil.PrintObject(cmd, object, out)
		}

		if err := kubectl.CreateOrUpdateAnnotation(cmdutil.GetFlagBool(cmd, cmdutil.ApplyAnnotationsFlag), hpa, cmdutil.InternalVersionJSONEncoder()); err != nil {
			return err
		}

		object, err = resource.NewHelper(hpa.Client, hpa.Mapping).Create(namespace, false, object)
		if err != nil {
			return err
		}

		count++
		if len(cmdutil.GetFlagString(cmd, "output")) > 0 {
			return cmdutil.PrintObject(cmd, object, out)
		}

		cmdutil.PrintSuccess(false, out, info.Object, cmdutil.GetDryRunFlag(cmd), "autoscaled")
		return nil
	})
	if err != nil {
		return err
	}
	if count == 0 {
		return fmt.Errorf("no objects passed to autoscale")
	}
	return nil
}

func validateFlags(cmd *cobra.Command) error {
	errs := []error{}
	max, min := cmdutil.GetFlagInt32(cmd, "max"), cmdutil.GetFlagInt32(cmd, "min")
	if max < 1 {
		errs = append(errs, fmt.Errorf("--max=MAXPODS is required and must be at least 1, max: %d", max))
	}
	if max < min {
		errs = append(errs, fmt.Errorf("--max=MAXPODS must be larger or equal to --min=MINPODS, max: %d, min: %d", max, min))
	}
	return utilerrors.NewAggregate(errs)
}
