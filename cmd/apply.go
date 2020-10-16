// Copyright (c) OpenFaaS Author(s) 2019. All rights reserved.
// Licensed under the MIT license. See LICENSE file in the project root for full license information.

package cmd

import (
	"errors"
	"fmt"
	"github.com/openfaas-incubator/ofc-bootstrap/pkg/charts"
	"github.com/openfaas-incubator/ofc-bootstrap/pkg/stack"
	"io/ioutil"
	"log"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/alexellis/arkade/pkg/helm"
	execute "github.com/alexellis/go-execute/pkg/v1"
	"github.com/alexellis/k3sup/pkg/config"
	"github.com/alexellis/k3sup/pkg/env"
	"github.com/openfaas-incubator/ofc-bootstrap/pkg/validators"

	"github.com/openfaas-incubator/ofc-bootstrap/pkg/types"
	"gopkg.in/yaml.v2"
)

func init() {
	rootCommand.AddCommand(applyCmd)

	applyCmd.Flags().StringArrayP("file", "f", []string{""}, "A number of init.yaml plan files")
	applyCmd.Flags().Bool("skip-sealedsecrets", false, "Skip SealedSecrets installation")
	applyCmd.Flags().Bool("skip-minio", false, "Skip Minio installation")
	applyCmd.Flags().Bool("skip-create-secrets", false, "Skip creating secrets")
	applyCmd.Flags().Bool("print-plan", false, "Print merged plan and exit")
	applyCmd.Flags().Bool("update-cloud", false, "set to true to only upgrade OFC components")
}

var applyCmd = &cobra.Command{
	Use:          "apply",
	Short:        "Apply configuration for OFC",
	RunE:         runApplyCommandE,
	SilenceUsage: true,
}

type InstallPreferences struct {
	SkipMinio         bool
	SkipSealedSecrets bool
	SkipCreateSecrets bool
}

func runApplyCommandE(command *cobra.Command, _ []string) error {

	prefs := InstallPreferences{}

	files, err := command.Flags().GetStringArray("file")
	if err != nil {
		return err
	}
	printPlan, _ := command.Flags().GetBool("print-plan")

	prefs.SkipMinio, _ = command.Flags().GetBool("skip-minio")
	prefs.SkipSealedSecrets, _ = command.Flags().GetBool("skip-sealedsecrets")
	prefs.SkipCreateSecrets, _ = command.Flags().GetBool("skip-create-secrets")

	if len(files) == 0 {
		return fmt.Errorf("Provide one or more --file arguments")
	}

	plans := []types.Plan{}

	log.Printf("Loading %d plans\n", len(files))
	for _, yamlFile := range files {

		yamlBytes, yamlErr := ioutil.ReadFile(yamlFile)
		if yamlErr != nil {
			return fmt.Errorf("loading --file %s gave error: %s", yamlFile, yamlErr.Error())
		}

		plan := types.Plan{}
		unmarshalErr := yaml.Unmarshal(yamlBytes, &plan)
		if unmarshalErr != nil {
			return fmt.Errorf("unmarshal of --file %s gave error: %s", yamlFile, unmarshalErr.Error())
		}
		log.Printf("%s loaded\n", yamlFile)
		plans = append(plans, plan)
	}

	planMerged, mergeErr := types.MergePlans(plans)

	if mergeErr != nil {
		return mergeErr
	}

	if printPlan {

		out, _ := yaml.Marshal(planMerged)
		fmt.Println(string(out))

		os.Exit(0)
	}

	plan := *planMerged

	var featuresErr error
	plan, featuresErr = filterFeatures(plan)
	if featuresErr != nil {
		return fmt.Errorf("error while retreiving features: %s", featuresErr.Error())
	}

	const helm3Version = "v3.1.2"
	os.Setenv("HELM_VERSION", helm3Version)

	userPath, err := config.InitUserDir()
	if err != nil {
		return err
	}

	clientArch, clientOS := env.GetClientArch()
	helmPathOut, err := helm.TryDownloadHelm(userPath, clientArch, clientOS, true)

	if err != nil {
		return err
	}

	log.Printf("helm3 at: %s\n", helmPathOut)

	additionalPaths := []string{helmPathOut}

	pathCurrent := os.Getenv("PATH")
	newPath := strings.Join(additionalPaths, ":") + ":" + pathCurrent
	os.Setenv("PATH", newPath)

	log.Printf("Validating tools available in PATH: %q\n", newPath)

	tools := []string{
		"kubectl version --client",
		"openssl version",
		"helm version -c",
		"faas-cli version",
	}

	validateToolsErr := validateTools(tools)

	if validateToolsErr != nil {
		panic(validateToolsErr)
	}

	if !prefs.SkipCreateSecrets {
		validateErr := validatePlan(plan)
		if validateErr != nil {
			panic(validateErr)
		}
	}

	fmt.Fprintf(os.Stdout, "Plan loaded from: %s\n", files)

	os.MkdirAll("tmp", 0700)
	ioutil.WriteFile("tmp/go.mod", []byte("\n"), 0700)

	fmt.Fprint(os.Stdout, "Validating registry credentials file\n")

	registryAuthErr := validateRegistryAuth(plan.Registry, plan.Secrets, plan.EnableECR)
	if registryAuthErr != nil {
		fmt.Fprint(os.Stderr, "error with registry credentials file. Please ensure it has been created correctly")
	}

	cloudOnly, err := command.Flags().GetBool("update-cloud")
	if err != nil {
		return err
	}


	if cloudOnly {
		err := cloudComponentsInstall(plan); if err != nil {
			return err
		}
		return nil
	}

	start := time.Now()
	err = process(plan, prefs)
	done := time.Since(start)

	if err != nil {
		return fmt.Errorf("plan failed after %f seconds, error: %s", done.Seconds(), err.Error())
	}

	fmt.Fprintf(os.Stdout, "Plan completed in %f seconds\n", done.Seconds())

	return nil
}

// Vars are variables parsed from flags
type Vars struct {
	YamlFile string
}

func taskGivesStdout(tool string) error {

	parts := strings.Split(tool, " ")

	args := []string{}

	if len(parts) > 0 {
		args = parts[1:]
	}

	task := execute.ExecTask{
		Command:     parts[0],
		Args:        args,
		StreamStdio: true,
	}

	res, err := task.Execute()
	if err != nil {
		return fmt.Errorf("could not run: '%s', error: %s", tool, err)
	}
	if len(res.Stdout) == 0 {
		return fmt.Errorf("error executing '%s', no output was given - tool is available in PATH", task.Command)
	}
	return nil
}

func validateTools(tools []string) error {

	for _, tool := range tools {
		err := taskGivesStdout(tool)
		if err != nil {
			return err
		}
	}

	return nil

}

func validateRegistryAuth(regEndpoint string, planSecrets []types.KeyValueNamespaceTuple, enableECR bool) error {
	if enableECR {
		return nil
	}
	for _, planSecret := range planSecrets {
		if planSecret.Name == "registry-secret" {
			confFileLocation := planSecret.Files[0].ExpandValueFrom()
			fileBytes, err := ioutil.ReadFile(confFileLocation)
			if err != nil {
				return err
			}
			return validators.ValidateRegistryAuth(regEndpoint, fileBytes)
		}
	}
	return nil
}

func validatePlan(plan types.Plan) error {
	for _, secret := range plan.Secrets {
		if featureEnabled(plan.Features, secret.Filters) {
			err := filesExists(secret.Files)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func filesExists(files []types.FileSecret) error {
	if len(files) > 0 {
		for _, file := range files {
			if len(file.ValueCommand) == 0 {
				if _, err := os.Stat(file.ExpandValueFrom()); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func process(plan types.Plan, prefs InstallPreferences) error {

	if plan.OpenFaaSCloudVersion == "" {
		plan.OpenFaaSCloudVersion = "master"
		fmt.Println("No openfaas_cloud_version set in init.yaml, using: master.")
	}

	nsErr := createNamespaces()
	if nsErr != nil {
		log.Println(nsErr)
		return nsErr
	}

	if err := helmRepoAdd("stable", "https://kubernetes-charts.storage.googleapis.com"); err != nil {
		log.Println(err.Error())
		return err
	}

	if err := helmRepoAdd("ingress-nginx", "https://kubernetes.github.io/ingress-nginx"); err != nil {
		log.Println(err.Error())
		return err
	}

	if err := helmRepoAdd("minio", "https://helm.min.io/"); err != nil {
		log.Println(err.Error())
		return err
	}

	if err := helmRepoUpdate(); err != nil {
		log.Println(err.Error())
		return err
	}

	installIngressErr := installIngressController(plan.Ingress)
	if installIngressErr != nil {
		log.Println(installIngressErr.Error())
		return installIngressErr
	}

	if !prefs.SkipCreateSecrets {
		createSecrets(plan)
	}

	saErr := patchFnServiceaccount()
	if saErr != nil {
		log.Println(saErr)
	}

	if !prefs.SkipMinio {
		minioErr := installMinio()
		if minioErr != nil {
			log.Println(minioErr)
		}
	}

	if plan.TLS {
		cmErr := installCertmanager()
		if cmErr != nil {
			log.Println(cmErr)
			return cmErr
		}
	}

	functionAuthErr := createFunctionsAuth()
	if functionAuthErr != nil {
		log.Println(functionAuthErr.Error())
	}

	ofErr := installOpenfaas(plan.ScaleToZero, plan.IngressOperator)
	if ofErr != nil {
		log.Println(ofErr)
	}

	retries := 260
	if plan.TLS {
		for i := 0; i < retries; i++ {
			log.Printf("Is cert-manager ready? %d/%d\n", i+1, retries)
			ready := certManagerReady()
			if ready {
				break
			}
			time.Sleep(time.Second * 2)
		}
	}

	fmt.Println("Creating stack.yml")

	planErr := stack.Apply(plan)
	if planErr != nil {
		log.Println(planErr)
		return planErr
	}

	if !prefs.SkipSealedSecrets {
		sealedSecretsErr := installSealedSecrets()
		if sealedSecretsErr != nil {
			log.Println(sealedSecretsErr)
			return sealedSecretsErr
		}

		pubCert := exportSealedSecretPubCert()
		writeErr := ioutil.WriteFile("tmp/pubcert.pem", []byte(pubCert), 0700)
		if writeErr != nil {
			log.Println(writeErr)
			return writeErr
		}
	}

	err := cloudComponentsInstall(plan)
	if err != nil {
		return err
	}

	return nil
}


func cloudComponentsInstall(plan types.Plan) error {
	cloneErr := cloneCloudComponents(plan.OpenFaaSCloudVersion)
	if cloneErr != nil {
		return cloneErr
	}

	ofcValues, err := writeOFCValuesYaml(plan)
	if err != nil {
		return err
	}

	err = charts.InstallOFC(ofcValues, plan.OpenFaaSCloudVersion); if err != nil {
		return err
	}

	deployErr := deployCloudComponents(plan)
	if deployErr != nil {
		return deployErr
	}
	return nil
}

func writeOFCValuesYaml(plan types.Plan) (map[string]string, error) {
	ofcOpts := types.OFCOpts{Options: map[string]string{}}

	ofcOpts.Set("networkPolicies.enabled", strconv.FormatBool(plan.NetworkPolicies))
	ofcOpts.Set("edgeAuth.enableOauth2", "false")

	if plan.EnableOAuth {
		ofcOpts.Set("edgeAuth.enableOauth2", "true")
		ofcOpts.Set("edgeAuth.oauthProvider", plan.SCM)
		ofcOpts.Set("edgeAuth.clientId", plan.OAuth.ClientId)
		ofcOpts.Set("edgeauth.oauthProviderBaseURL", plan.OAuth.OAuthProviderBaseURL)
	}

	ofcOpts.Set("global.enableECR", strconv.FormatBool(plan.EnableECR))

	if plan.TLS {
		ofcOpts.Set("tls.issuerType", plan.TLSConfig.IssuerType)
		ofcOpts.Set("tls.enabled", "true")
		ofcOpts.Set("tls.email", plan.TLSConfig.Email)
		ofcOpts.Set("tls.dnsService", plan.TLSConfig.DNSService)
		switch plan.TLSConfig.DNSService {
		case types.CloudDNS:
			ofcOpts.Set("tls.clouddns.projectID", plan.TLSConfig.ProjectID)
		case types.Cloudflare:
			ofcOpts.Set("tls.cloudflare.email", plan.TLSConfig.Email)
			ofcOpts.Set("tls.cloudflare.projectID", plan.TLSConfig.ProjectID)
		case types.Route53:
			ofcOpts.Set("tls.route53.accessKeyID", plan.TLSConfig.AccessKeyID)
			ofcOpts.Set("tls.route53.region", plan.TLSConfig.Region)
		case types.DigitalOcean:
			// No special config for DO DNS
		default:
			log.Fatalf("dns service not recognised: %s", plan.TLSConfig.DNSService)
		}

	} else {
		ofcOpts.Set("tls.enabled", "false")
	}

	ofcOpts.Set("customers.customerSecret", strconv.FormatBool(plan.CustomersSecret))
	ofcOpts.Set("customers.url", plan.CustomersURL)
	if len(plan.CustomersURL) == 0 && !plan.CustomersSecret {
		return nil, errors.New("unable to continue without a customers secret or url")
	}

	ofcOpts.Set("global.rootDomain", plan.RootDomain)

	ofcOpts.Set("ingress.maxConnections", plan.IngressConfig.MaxConnections)
	ofcOpts.Set("ingress.requestsPerMinute", plan.IngressConfig.RequestsPerMinute)

	return ofcOpts.Options, nil
}

func helmRepoAdd(name, repo string) error {
	log.Printf("Adding %s helm repo\n", name)

	task := execute.ExecTask{
		Command:     "helm",
		Args:        []string{"repo", "add", name, repo},
		StreamStdio: true,
	}

	taskRes, taskErr := task.Execute()

	if taskErr != nil {
		return taskErr
	}

	if len(taskRes.Stderr) > 0 {
		log.Println(taskRes.Stderr)
	}

	return nil
}

func helmRepoUpdate() error {
	log.Println("Updating helm repos")

	task := execute.ExecTask{
		Command:     "helm",
		Args:        []string{"repo", "update"},
		StreamStdio: true,
	}

	taskRes, taskErr := task.Execute()

	if taskErr != nil {
		return taskErr
	}

	if len(taskRes.Stderr) > 0 {
		log.Println(taskRes.Stderr)
	}

	return nil
}

func createFunctionsAuth() error {
	log.Println("Creating secrets for functions to consume")

	task := execute.ExecTask{
		Command:     "scripts/create-functions-auth.sh",
		Shell:       true,
		StreamStdio: true,
	}

	taskRes, err := task.Execute()

	if err != nil {
		return err
	}

	if len(taskRes.Stderr) > 0 {
		log.Println(taskRes.Stderr)
	}

	return nil
}

func installIngressController(ingress string) error {
	log.Println("Creating Ingress Controller")

	var env []string
	if ingress == "host" {
		env = append(env, "ADDITIONAL_SET=,controller.hostNetwork=true,controller.daemonset.useHostPort=true,dnsPolicy=ClusterFirstWithHostNet,controller.kind=DaemonSet")
	}

	task := execute.ExecTask{
		Command:     "scripts/install-nginx.sh",
		Shell:       true,
		Env:         env,
		StreamStdio: true,
	}

	res, err := task.Execute()

	if err != nil {
		return err
	}

	log.Println(res.ExitCode, res.Stdout, res.Stderr)

	return nil
}

func installSealedSecrets() error {
	log.Println("Creating SealedSecrets")

	task := execute.ExecTask{
		Command:     "scripts/install-sealedsecrets.sh",
		Shell:       true,
		StreamStdio: true,
	}

	taskRes, err := task.Execute()

	if err != nil {
		return err
	}

	if len(taskRes.Stderr) > 0 {
		log.Println(taskRes.Stderr)
	}

	return nil
}

func installOpenfaas(scaleToZero, ingressOperator bool) error {
	log.Println("Creating OpenFaaS")

	task := execute.ExecTask{
		Command: "scripts/install-openfaas.sh",
		Shell:   true,
		Env: []string{
			fmt.Sprintf("FAAS_IDLER_DRY_RUN=%v", strconv.FormatBool(!scaleToZero)),
			fmt.Sprintf("INSTALL_INGRESS_OPERATOR=%v", strconv.FormatBool(ingressOperator)),
		},
		StreamStdio: true,
	}

	res, err := task.Execute()

	if err != nil {
		return err
	}

	log.Println(res.ExitCode, res.Stdout, res.Stderr)

	return nil
}

func installMinio() error {
	log.Println("Creating Minio")

	task := execute.ExecTask{
		Command:     "scripts/install-minio.sh",
		Shell:       true,
		StreamStdio: true,
	}

	taskRes, err := task.Execute()

	if err != nil {
		return err
	}

	if len(taskRes.Stderr) > 0 {
		log.Println(taskRes.Stderr)
	}

	return nil
}

func patchFnServiceaccount() error {
	log.Println("Patching openfaas-fn serviceaccount for pull secrets")

	task := execute.ExecTask{
		Command:     "scripts/patch-fn-serviceaccount.sh",
		Shell:       true,
		StreamStdio: true,
	}

	taskRes, err := task.Execute()

	if err != nil {
		return err
	}

	if len(taskRes.Stderr) > 0 {
		log.Println(taskRes.Stderr)
	}
	return nil
}

func installCertmanager() error {
	log.Println("Creating Cert-Manager")

	task := execute.ExecTask{
		Command:     "scripts/install-cert-manager.sh",
		Shell:       true,
		StreamStdio: true,
	}

	res, err := task.Execute()

	if err != nil {
		return err
	}

	log.Println(res.ExitCode, res.Stdout, res.Stderr)

	return nil
}

func createNamespaces() error {
	log.Println("Creating namespaces")

	task := execute.ExecTask{
		Command:     "scripts/create-namespaces.sh",
		Shell:       true,
		StreamStdio: true,
	}

	res, err := task.Execute()

	if err != nil {
		return err
	}

	log.Println(res.ExitCode, res.Stdout, res.Stderr)

	return nil
}

func createSecrets(plan types.Plan) error {

	for _, secret := range plan.Secrets {
		if featureEnabled(plan.Features, secret.Filters) {
			fmt.Printf("Creating secret: %s\n", secret.Name)

			command := types.BuildSecretTask(secret)
			fmt.Printf("Secret - %s %s\n", command.Command, command.Args)
			res, err := command.Execute()

			if err != nil {
				log.Println(err)
			}

			fmt.Println(res)
		}
	}

	return nil
}

func exportSealedSecretPubCert() string {

	task := execute.ExecTask{
		Command:     "./scripts/export-sealed-secret-pubcert.sh",
		Shell:       true,
		StreamStdio: true,
	}

	res, err := task.Execute()
	fmt.Println("secrets cert", res.ExitCode, res.Stdout, res.Stderr, err)
	return res.Stdout
}

func certManagerReady() bool {
	task := execute.ExecTask{
		Command:     "./scripts/get-cert-manager.sh",
		Shell:       true,
		StreamStdio: true,
	}

	res, err := task.Execute()
	fmt.Println("cert-manager", res.ExitCode, res.Stdout, res.Stderr, err)
	return res.Stdout == "True"
}

func cloneCloudComponents(tag string) error {
	task := execute.ExecTask{
		Command: "./scripts/clone-cloud-components.sh",
		Shell:   true,
		Env: []string{
			fmt.Sprintf("TAG=%v", tag),
		},
		StreamStdio: true,
	}

	_, err := task.Execute()
	if err != nil {
		return err
	}

	return nil
}

func deployCloudComponents(plan types.Plan) error {

	authEnv := ""
	if plan.EnableOAuth {
		authEnv = "ENABLE_OAUTH=true"
	}

	gitlabEnv := ""
	if plan.SCM == "gitlab" {
		gitlabEnv = "GITLAB=true"
	}

	networkPoliciesEnv := ""
	if plan.NetworkPolicies {
		networkPoliciesEnv = "ENABLE_NETWORK_POLICIES=true"
	}

	enableECREnv := ""
	if plan.EnableECR {
		enableECREnv = "ENABLE_AWS_ECR=true"
	}

	task := execute.ExecTask{
		Command: "./scripts/deploy-cloud-components.sh",
		Shell:   true,
		Env: []string{authEnv,
			gitlabEnv,
			networkPoliciesEnv,
			enableECREnv,
		},
		StreamStdio: true,
	}

	res, err := task.Execute()
	if err != nil {
		return err
	}

	fmt.Println(res)

	return nil
}

func featureEnabled(features []string, secretFeatures []string) bool {
	for _, feature := range features {
		for _, secretFeature := range secretFeatures {
			if feature == secretFeature {
				return true
			}
		}
	}
	return false
}

func filterFeatures(plan types.Plan) (types.Plan, error) {
	var err error

	plan.Features = append(plan.Features, types.DefaultFeature)

	if plan.EnableECR == true {
		plan.Features = append(plan.Features, types.ECRFeature)
	}

	plan, err = filterGitRepositoryManager(plan)
	if err != nil {
		return plan, fmt.Errorf("Error while filtering features: %s", err.Error())
	}

	if plan.TLS == true {
		plan, err = filterDNSFeature(plan)
		if err != nil {
			return plan, fmt.Errorf("Error while filtering features: %s", err.Error())
		}
	}

	if plan.EnableOAuth == true {
		plan.Features = append(plan.Features, types.Auth)
	}

	return plan, err
}

func filterDNSFeature(plan types.Plan) (types.Plan, error) {
	if plan.TLSConfig.DNSService == types.DigitalOcean {
		plan.Features = append(plan.Features, types.DODNS)
	} else if plan.TLSConfig.DNSService == types.CloudDNS {
		plan.Features = append(plan.Features, types.GCPDNS)
	} else if plan.TLSConfig.DNSService == types.Route53 {
		plan.Features = append(plan.Features, types.Route53DNS)
	} else if plan.TLSConfig.DNSService == types.Cloudflare {
		plan.Features = append(plan.Features, types.CloudflareDNS)
	} else {
		return plan, fmt.Errorf("Error unavailable DNS service provider: %s", plan.TLSConfig.DNSService)
	}
	return plan, nil
}

func filterGitRepositoryManager(plan types.Plan) (types.Plan, error) {
	if plan.SCM == types.GitLabSCM {
		plan.Features = append(plan.Features, types.GitLabFeature)
	} else if plan.SCM == types.GitHubSCM {
		plan.Features = append(plan.Features, types.GitHubFeature)
	} else {
		return plan, fmt.Errorf("Error unsupported Git repository manager: %s", plan.SCM)
	}
	return plan, nil
}
