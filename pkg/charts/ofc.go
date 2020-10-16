package charts

import (
	"github.com/alexellis/arkade/pkg/apps"
	ark "github.com/alexellis/arkade/pkg/types"
)

func InstallOFC(overrides map[string]string, ofcVersion string) error {

	ofcOpts := ark.DefaultInstallOptions()
	ofcOpts.WithNamespace("openfaas")
	ofcOpts.WithHelmRepo("")
	ofcOpts.WithHelmURL("")
	ofcOpts.Helm.Repo.Version = ofcVersion
	ofcOpts.WithOverrides(overrides)

	_, err := apps.MakeInstallChart(ofcOpts)
		return err
}