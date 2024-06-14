//go:build e2e
// +build e2e

/*
Copyright 2024 IONOS Cloud.

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

// Package e2e offers end-to-end tests for the Cluster API IONOS Cloud provider.
package e2e

import (
	"context"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/v1beta1"
	"sigs.k8s.io/cluster-api/test/framework"
	"sigs.k8s.io/cluster-api/test/framework/bootstrap"
	"sigs.k8s.io/cluster-api/test/framework/clusterctl"
	"sigs.k8s.io/cluster-api/test/framework/ginkgoextensions"
	ctrl "sigs.k8s.io/controller-runtime"

	infrav1 "github.com/ionos-cloud/cluster-api-provider-ionoscloud/api/v1alpha1"
)

// Test suite flags
var (
	// configPath is the path to the e2e config file.
	configPath string

	// useExistingCluster instructs the test to use the current cluster instead of creating a new one (default discovery rules apply).
	useExistingCluster bool

	// artifactFolder is the folder to store e2e test artifacts.
	artifactFolder string

	// clusterctlConfig is the file which tests will use as a clusterctl config.
	// If it is not set, a local clusterctl repository (including a clusterctl config) will be created automatically.
	clusterctlConfig string

	// alsoLogToFile enables additional logging to the 'ginkgo-log.txt' file in the artifact folder.
	// These logs also contain timestamps.
	alsoLogToFile bool

	// skipCleanup prevents clean-up of test resources e.g. for debug purposes.
	skipCleanup bool
)

// Test suite global vars.
var (
	ctx = ctrl.SetupSignalHandler()

	// cloudEnv manages required resources for the cloud environment where the resources are gonna be created.
	cloudEnv = &ionosCloudEnv{}

	// watchesCtx is used in log streaming to be able to get canceled via cancelWatches after ending the test suite.
	watchesCtx, cancelWatches = context.WithCancel(ctx)

	// e2eConfig to be used for this test, read from configPath.
	e2eConfig *clusterctl.E2EConfig

	// clusterctlConfigPath to be used for this test, created by generating a clusterctl local repository
	// with the providers specified in the configPath.
	clusterctlConfigPath string

	// bootstrapClusterProvider manages provisioning of the bootstrap cluster to be used for the e2e tests.
	// Please note that provisioning will be skipped if e2e.use-existing-cluster is provided.
	bootstrapClusterProvider bootstrap.ClusterProvider

	// bootstrapClusterProxy allows to interact with the bootstrap cluster to be used for the e2e tests.
	bootstrapClusterProxy framework.ClusterProxy
)

func init() {
	flag.StringVar(&configPath, "e2e.config", "", "path to the e2e config file")
	flag.StringVar(&artifactFolder, "e2e.artifacts-folder", "", "folder where e2e test artifact should be stored")
	flag.StringVar(&clusterctlConfig, "e2e.clusterctl-config", "",
		"file which tests will use as a clusterctl config. If it is not set, a local clusterctl"+
			"repository (including a clusterctl config) will be created automatically.")
	flag.BoolVar(&useExistingCluster, "e2e.use-existing-cluster", false,
		"if true, the test uses the current cluster instead of creating a new one (default discovery rules apply)")
	flag.BoolVar(&skipCleanup, "e2e.skip-resource-cleanup", false, "if true, the resource cleanup after tests will be skipped")
	flag.BoolVar(&alsoLogToFile, "e2e.also-log-to-file", true, "if true, ginkgo logs are additionally written to the `ginkgo-log.txt` file in the artifacts folder (including timestamps)")
}

func TestE2E(t *testing.T) {
	if testing.Short() {
		t.Skip("Skipping as only short tests should run")
	}
	g := NewWithT(t)

	// ensure the artifacts folder exists
	g.Expect(os.MkdirAll(artifactFolder, 0755)).To(Succeed(), "Invalid test suite argument. Can't create e2e.artifacts-folder %q", artifactFolder)

	if alsoLogToFile {
		w, err := ginkgoextensions.EnableFileLogging(filepath.Join(artifactFolder, "ginkgo-log.txt"))
		g.Expect(err).ToNot(HaveOccurred())
		defer w.Close()
	}

	RegisterFailHandler(Fail)
	RunSpecs(t, "E2E test suite")
}

// Using a SynchronizedBeforeSuite for controlling how to create resources shared across ParallelNodes (~ginkgo threads).
// The local clusterctl repository & the bootstrap cluster are created once and shared across all the tests.
var _ = SynchronizedBeforeSuite(func() []byte {
	// Before all ParallelNodes.

	Expect(configPath).To(BeAnExistingFile(), "Invalid test suite argument. e2e.config should be an existing file.")

	By("Initializing a runtime.Scheme with all the GVK relevant for this test")
	scheme := initScheme()

	Byf("Loading the e2e test configuration from %q", configPath)
	e2eConfig = loadE2EConfig(configPath)

	if clusterctlConfig == "" {
		Byf("Creating a clusterctl local repository into %q", artifactFolder)
		clusterctlConfigPath = createClusterctlLocalRepository(e2eConfig, filepath.Join(artifactFolder, "repository"))
	} else {
		Byf("Using existing clusterctl config %q", clusterctlConfig)
		clusterctlConfigPath = clusterctlConfig
	}

	By("Setting up the bootstrap cluster")
	bootstrapClusterProvider, bootstrapClusterProxy = setupBootstrapCluster(scheme)

	By("Initializing the cloud environment")
	cloudEnv.setup()

	By("Initializing the bootstrap cluster")
	initBootstrapCluster()

	return []byte(
		strings.Join([]string{
			artifactFolder,
			configPath,
			clusterctlConfigPath,
			bootstrapClusterProxy.GetKubeconfigPath(),
		}, ","),
	)
}, func(data []byte) {
	// Before each ParallelNode.
	parts := strings.Split(string(data), ",")
	Expect(parts).To(HaveLen(4))

	artifactFolder = parts[0]
	configPath = parts[1]
	clusterctlConfigPath = parts[2]
	kubeconfigPath := parts[3]

	e2eConfig = loadE2EConfig(configPath)
	bootstrapClusterProxy = framework.NewClusterProxy("bootstrap", kubeconfigPath, initScheme(), framework.WithMachineLogCollector(framework.DockerLogCollector{}))
})

// Using a SynchronizedAfterSuite for controlling how to delete resources shared across ParallelNodes (~ginkgo threads).
// The bootstrap cluster is shared across all the tests, so it should be deleted only after all ParallelNodes completes.
// The local clusterctl repository is preserved like everything else created into the artifact folder.
var _ = SynchronizedAfterSuite(func() {
	// After each ParallelNode.
}, func() {
	// After all ParallelNodes.
	By("Dumping logs from the bootstrap cluster")
	dumpBootstrapClusterLogs()

	By("Tearing down the management cluster")
	if !skipCleanup {
		tearDown()
	}
})

func initScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	framework.TryAddDefaultSchemes(s)
	Expect(infrav1.AddToScheme(s)).To(Succeed())
	return s
}

func loadE2EConfig(configPath string) *clusterctl.E2EConfig {
	config := clusterctl.LoadE2EConfig(ctx, clusterctl.LoadE2EConfigInput{ConfigPath: configPath})
	Expect(config).ToNot(BeNil(), "Failed to load E2E config from %s", configPath)
	return config
}

func createClusterctlLocalRepository(config *clusterctl.E2EConfig, repositoryFolder string) string {
	createRepositoryInput := clusterctl.CreateRepositoryInput{
		E2EConfig:        e2eConfig,
		RepositoryFolder: repositoryFolder,
	}

	// Ensuring a CNI file is defined in the config and register a FileTransformation to
	// inject the referenced file in place of the CNI_RESOURCES envSubst variable.
	Expect(config.Variables).To(HaveKey(CNIPath), "Missing %s variable in the config", CNIPath)
	cniPath := config.GetVariable(CNIPath)
	Expect(cniPath).To(BeAnExistingFile(), "The %s variable should resolve to an existing file", CNIPath)

	createRepositoryInput.RegisterClusterResourceSetConfigMapTransformation(cniPath, CNIResources)
	clusterctlConfig := clusterctl.CreateRepository(ctx, createRepositoryInput)
	Expect(clusterctlConfig).To(BeAnExistingFile(), "The clusterctl config file does not exists in the local repository %s", repositoryFolder)
	return clusterctlConfig
}

func setupBootstrapCluster(scheme *runtime.Scheme) (bootstrap.ClusterProvider, framework.ClusterProxy) {
	var clusterProvider bootstrap.ClusterProvider
	kubeconfigPath := ""
	if !useExistingCluster {
		clusterProvider = bootstrap.CreateKindBootstrapClusterAndLoadImages(ctx, bootstrap.CreateKindBootstrapClusterAndLoadImagesInput{
			KubernetesVersion: e2eConfig.GetVariable(KubernetesVersion),
			Name:              e2eConfig.ManagementClusterName,
			Images:            e2eConfig.Images,
			LogFolder:         filepath.Join(artifactFolder, "kind"),
		})
		Expect(clusterProvider).ToNot(BeNil(), "Failed to create a bootstrap cluster")

		kubeconfigPath = clusterProvider.GetKubeconfigPath()
		Expect(kubeconfigPath).To(BeAnExistingFile(), "Failed to get the kubeconfig file for the bootstrap cluster")
	} else {
		By("Using an existing bootstrap cluster")
	}

	clusterProxy := framework.NewClusterProxy("bootstrap", kubeconfigPath, scheme)
	Expect(clusterProxy).ToNot(BeNil(), "Failed to get a bootstrap cluster proxy")
	return clusterProvider, clusterProxy
}

func initBootstrapCluster() {
	clusterctl.InitManagementClusterAndWatchControllerLogs(watchesCtx, clusterctl.InitManagementClusterAndWatchControllerLogsInput{
		ClusterProxy:            bootstrapClusterProxy,
		ClusterctlConfigPath:    clusterctlConfigPath,
		InfrastructureProviders: e2eConfig.InfrastructureProviders(),
		LogFolder:               filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName()),
	}, e2eConfig.GetIntervals(bootstrapClusterProxy.GetName(), "wait-controllers")...)
}

func dumpBootstrapClusterLogs() {
	if bootstrapClusterProxy == nil {
		return
	}
	clusterLogCollector := bootstrapClusterProxy.GetLogCollector()
	if clusterLogCollector == nil {
		return
	}

	nodes, err := bootstrapClusterProxy.GetClientSet().CoreV1().Nodes().List(ctx, metav1.ListOptions{})
	if err != nil {
		fmt.Printf("Failed to get nodes for the bootstrap cluster: %v\n", err)
		return
	}

	for i := range nodes.Items {
		nodeName := nodes.Items[i].GetName()
		err := clusterLogCollector.CollectMachineLog(
			ctx,
			bootstrapClusterProxy.GetClient(),
			// The bootstrap cluster is not expected to be a CAPI cluster, so in order to reuse the logCollector,
			// we create a fake machine that wraps the node.
			// NOTE: This assumes a naming convention between machine and nodes, which e.g. applies to the bootstrap
			// clusters generated with kind. This might not work if you are using an existing bootstrap cluster
			// provided by other means
			&clusterv1.Machine{
				Spec:       clusterv1.MachineSpec{ClusterName: nodeName},
				ObjectMeta: metav1.ObjectMeta{Name: nodeName},
			},
			filepath.Join(artifactFolder, "clusters", bootstrapClusterProxy.GetName(), "machines", nodeName),
		)
		if err != nil {
			fmt.Printf("Failed to get logs for the bootstrap cluster node %s: %v\n", nodeName, err)
		}
	}
}

func tearDown() {
	cancelWatches()
	if bootstrapClusterProxy != nil {
		bootstrapClusterProxy.Dispose(ctx)
	}
	if bootstrapClusterProvider != nil {
		bootstrapClusterProvider.Dispose(ctx)
	}
	if cloudEnv != nil {
		cloudEnv.teardown()
	}
}