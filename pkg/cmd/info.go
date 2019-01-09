package cmd

import (
	"fmt"
	"os"
	"path"

	"github.com/spf13/cobra"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericclioptions/printers"
	"k8s.io/cli-runtime/pkg/genericclioptions/resource"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"

	"io/ioutil"

	configv1 "github.com/openshift/api/config/v1"
)

var (
	infoExample = `
	# Collect debugging data for the "openshift-apiserver-operator"
	%[1]s info clusteroperator/openshift-apiserver-operator

	# Collect debugging data for all clusteroperators
	%[1]s info clusteroperator
`
)

type InfoOptions struct {
	printFlags  *genericclioptions.PrintFlags
	configFlags *genericclioptions.ConfigFlags

	discoveryClient discovery.CachedDiscoveryInterface
	dynamicClient   dynamic.Interface

	printer printers.ResourcePrinter
	builder *resource.Builder
	args    []string

	// directory where all gathered data will be stored
	baseDir string
	// whether or not to allow writes to an existing and populated base directory
	overwrite bool

	genericclioptions.IOStreams
}

func NewInfoOptions(streams genericclioptions.IOStreams) *InfoOptions {
	return &InfoOptions{
		printFlags:  genericclioptions.NewPrintFlags("gathered").WithDefaultOutput("yaml"),
		configFlags: genericclioptions.NewConfigFlags(),
		IOStreams:   streams,
	}
}

func NewCmdInfo(parentName string, streams genericclioptions.IOStreams) *cobra.Command {
	o := NewInfoOptions(streams)

	cmd := &cobra.Command{
		Use:          "info <operator> [flags]",
		Short:        "Gather debugging data for a given cluster operator",
		Example:      fmt.Sprintf(infoExample, parentName),
		SilenceUsage: true,
		RunE: func(c *cobra.Command, args []string) error {
			if err := o.Complete(c, args); err != nil {
				return err
			}
			if err := o.Validate(); err != nil {
				return err
			}
			if err := o.Run(); err != nil {
				return err
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&o.baseDir, "base-dir", "must-gather", "Root directory used for storing all gathered cluster operator data. Defaults to $(PWD)/must-gather")
	cmd.Flags().BoolVar(&o.overwrite, "overwrite", false, "If true, allow this command to write to an existing location with previous data present")

	o.printFlags.AddFlags(cmd)
	return cmd
}

func (o *InfoOptions) Complete(cmd *cobra.Command, args []string) error {
	o.args = args

	config, err := o.configFlags.ToRESTConfig()
	if err != nil {
		return err
	}

	o.dynamicClient, err = dynamic.NewForConfig(config)
	if err != nil {
		return err
	}

	o.discoveryClient, err = o.configFlags.ToDiscoveryClient()
	if err != nil {
		return err
	}

	o.printer, err = o.printFlags.ToPrinter()
	if err != nil {
		return err
	}

	o.builder = resource.NewBuilder(o.configFlags)
	return nil
}

func (o *InfoOptions) Validate() error {
	if len(o.args) != 1 {
		return fmt.Errorf("exactly 1 argument (operator name) is supported")
	}
	if len(o.baseDir) == 0 {
		return fmt.Errorf("--base-dir must not be empty")
	}
	return nil
}

func (o *InfoOptions) Run() error {
	r := o.builder.
		Unstructured().
		ResourceTypeOrNameArgs(true, o.args...).
		Flatten().
		Latest().Do()

	infos, err := r.Infos()
	if err != nil {
		return err
	}

	if err := o.ensureDirectoryViable(o.baseDir, o.overwrite); err != nil {
		return err
	}

	// first, gather config.openshift.io resource data
	if err := o.gatherConfigResourceData(path.Join(o.baseDir, "/resources/config.openshift.io")); err != nil {
		// TODO: aggregate error
		return err
	}

	for _, info := range infos {
		// TODO: aggregate errors
		if configv1.GroupName != info.Mapping.GroupVersionKind.Group {
			return fmt.Errorf("unexpected resource API group %q. Expected %q", info.Mapping.GroupVersionKind.Group, configv1.GroupName)
		}
		if info.Mapping.Resource.Resource != "clusteroperators" {
			return fmt.Errorf("unsupported resource type, must be %q", "clusteroperators")
		}

		// save clusteroperator resources
		if err := o.gatherClusterOperatorResource(path.Join(o.baseDir, "/resources"), info); err != nil {
			return err
		}

	}

	return nil
}

// ensureDirectoryViable returns an error if the given path:
// 1. already exists AND is a file (not a directory)
// 2. already exists AND is NOT empty
// 3. an IO error occurs
func (o *InfoOptions) ensureDirectoryViable(dirPath string, allowDataOverride bool) error {
	baseDirInfo, err := os.Stat(dirPath)
	if err != nil && os.IsNotExist(err) {
		// no error, directory simply does not exist yet
		return nil
	}
	if err != nil {
		return err
	}

	if !baseDirInfo.IsDir() {
		return fmt.Errorf("%q exists and is a file", dirPath)
	}
	files, err := ioutil.ReadDir(dirPath)
	if err != nil {
		return err
	}
	if len(files) > 0 && !o.overwrite {
		return fmt.Errorf("%q exists and is not empty. Pass --overwrite to allow data overwrites", dirPath)
	}
	return nil
}

func (o *InfoOptions) gatherClusterOperatorResource(destDir string, info *resource.Info) error {
	// ensure destination path exists
	if err := os.MkdirAll(destDir, os.ModePerm); err != nil {
		return err
	}

	filename := fmt.Sprintf("%s.yaml", info.Name)
	dest, err := os.OpenFile(path.Join(destDir, "/"+filename), os.O_RDWR|os.O_CREATE, 0755)
	if err != nil {
		return err
	}
	defer dest.Close()

	if err := o.printer.PrintObj(info.Object, dest); err != nil {
		return err
	}

	return nil
}

func (o *InfoOptions) gatherConfigResourceData(destDir string) error {
	// ensure destination path exists
	if err := os.MkdirAll(destDir, os.ModePerm); err != nil {
		return err
	}

	resources, err := retrieveConfigResourceNames(o.discoveryClient)
	if err != nil {
		return err
	}

	for _, resource := range resources {
		resourceList, err := o.dynamicClient.Resource(resource).List(metav1.ListOptions{})
		if err != nil {
			// TODO: aggregate errors, do not fail on a single one in order to collect some metrics despite failures
			return err
		}

		objToPrint := runtime.Object(resourceList)

		err = func() error {
			filename := fmt.Sprintf("%s.yaml", resource.Resource)
			dest, err := os.OpenFile(path.Join(destDir, "/"+filename), os.O_RDWR|os.O_CREATE, 0755)
			if err != nil {
				return err
			}
			defer dest.Close()

			if err := o.printer.PrintObj(objToPrint, dest); err != nil {
				return err
			}

			return nil
		}()
		if err != nil {
			// TODO: aggregate this error
			return err
		}
	}

	return nil
}

func retrieveConfigResourceNames(discoveryClient discovery.CachedDiscoveryInterface) ([]schema.GroupVersionResource, error) {
	lists, err := discoveryClient.ServerPreferredResources()
	if err != nil {
		return nil, err
	}

	resources := []schema.GroupVersionResource{}
	for _, list := range lists {
		if len(list.APIResources) == 0 {
			continue
		}
		gv, err := schema.ParseGroupVersion(list.GroupVersion)
		if err != nil {
			continue
		}
		for _, resource := range list.APIResources {
			if len(resource.Verbs) == 0 {
				continue
			}
			// filter groups outside of config.openshift.io
			if gv.Group != configv1.GroupName {
				continue
			}
			resources = append(resources, schema.GroupVersionResource{Group: gv.Group, Version: gv.Version, Resource: resource.Name})
		}
	}

	return resources, nil
}
