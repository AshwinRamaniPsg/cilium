// Copyright 2017-2020 Authors of Cilium
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

package helpers

import (
	"bytes"
	"context"
	"fmt"
	"html/template"
	"io"
	"io/ioutil"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cilium/cilium/pkg/rand"
	"github.com/cilium/cilium/pkg/versioncheck"
	"github.com/cilium/cilium/test/config"
	ginkgoext "github.com/cilium/cilium/test/ginkgo-ext"

	go_version "github.com/blang/semver"
	"github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"golang.org/x/sys/unix"
)

var (
	// ensure that our random numbers are seeded differently on each run
	randGen              = rand.NewSafeRand(time.Now().UnixNano())
	runningCiliumVersion string
)

// IsRunningOnJenkins detects if the currently running Ginkgo application is
// most likely running in a Jenkins environment. Returns true if certain
// environment variables that are present in Jenkins jobs are set, false
// otherwise.
func IsRunningOnJenkins() bool {
	result := true

	env := []string{"JENKINS_HOME", "NODE_NAME"}

	for _, varName := range env {
		if val := os.Getenv(varName); val == "" {
			result = false
			log.Infof("build is not running on Jenkins; environment variable '%v' is not set", varName)
		}
	}
	return result
}

// SetCiliumVersion sets the currently running cilium version
// and returns a function that unsets it.
func SetRunningCiliumVersion(v string) func() {
	runningCiliumVersion = v
	return func() {
		runningCiliumVersion = ""
	}
}

// GetRunningCiliumVersion gets the currently running cilium version.
func GetRunningCiliumVersion() string {
	return runningCiliumVersion
}

// HasNewServiceOutput checks to see if the current running cilium
// version uses the old style service output (e.g. "0.0.0.0:53") vs
// the new style (e.g. "0.0.0.0:53/TCP").
func HasNewServiceOutput(ver string) bool {
	cst, err := versioncheck.Version(ver)
	// If the version is not parseable it is probably
	// someone's custom build  or not set.
	// Either way, it is probably using the new output
	// format.
	if err != nil {
		return true
	}
	return versioncheck.MustCompile(">=1.9.0")(cst)
}

// Sleep sleeps for the specified duration in seconds
func Sleep(delay time.Duration) {
	time.Sleep(delay * time.Second)
}

// CountValues returns the count of the occurrences of key in data, as well as
// the length of data.
func CountValues(key string, data []string) (int, int) {
	var result int

	for _, x := range data {
		if x == key {
			result++
		}
	}
	return result, len(data)
}

// MakeUID returns a randomly generated string.
func MakeUID() string {
	return fmt.Sprintf("%08x", randGen.Uint32())
}

// RenderTemplate renders a text/template string into a buffer.
// Returns eturn an error if the template cannot be validated.
func RenderTemplate(tmplt string) (*bytes.Buffer, error) {
	t, err := template.New("").Parse(tmplt)
	if err != nil {
		return nil, err
	}
	content := new(bytes.Buffer)
	err = t.Execute(content, nil)
	if err != nil {
		return nil, err
	}
	return content, nil
}

// TimeoutConfig represents the configuration for the timeout of a command.
type TimeoutConfig struct {
	Ticker  time.Duration // Check interval
	Timeout time.Duration // Limit for how long to spend in the command
}

// Validate ensuires that the parameters for the TimeoutConfig are reasonable
// for running in tests.
func (c *TimeoutConfig) Validate() error {
	if c.Timeout < 5*time.Second {
		return fmt.Errorf("Timeout too short (must be at least 5 seconds): %v", c.Timeout)
	}
	if c.Ticker == 0 {
		c.Ticker = 5 * time.Second
	} else if c.Ticker < time.Second {
		return fmt.Errorf("Timeout config Ticker interval too short (must be at least 1 second): %v", c.Ticker)
	}
	return nil
}

// WithTimeout executes body using the time interval specified in config until
// the timeout in config is reached. Returns an error if the timeout is
// exceeded for body to execute successfully.
func WithTimeout(body func() bool, msg string, config *TimeoutConfig) error {
	err := RepeatUntilTrue(body, config)
	if err != nil {
		return fmt.Errorf("%s: %s", msg, err)
	}

	return nil
}

// RepeatUntilTrueDefaultTimeout calls RepeatUntilTrue with the default timeout
// HelperTimeout
func RepeatUntilTrueDefaultTimeout(body func() bool) error {
	return RepeatUntilTrue(body, &TimeoutConfig{Timeout: HelperTimeout})
}

// RepeatUntilTrue repeatedly calls body until body returns true or the timeout
// expires
func RepeatUntilTrue(body func() bool, config *TimeoutConfig) error {
	if err := config.Validate(); err != nil {
		return err
	}

	bodyChan := make(chan bool, 1)

	asyncBody := func(ch chan bool) {
		defer ginkgo.GinkgoRecover()
		success := body()
		ch <- success
		if success {
			close(ch)
		}
	}

	go asyncBody(bodyChan)

	done := time.After(config.Timeout)
	ticker := time.NewTicker(config.Ticker)
	defer ticker.Stop()
	for {
		select {
		case success := <-bodyChan:
			if success {
				return nil
			}
			// Provide some form of rate-limiting here before running next
			// execution in case body() returns at a fast rate.
			select {
			case <-ticker.C:
				go asyncBody(bodyChan)
			}
		case <-done:
			return fmt.Errorf("%s timeout expired", config.Timeout)
		}
	}
}

// WithContext executes body with the given frequency. The function
// f is executed until bool returns true or the given context signalizes Done.
// `f` should stop if context is canceled.
func WithContext(ctx context.Context, f func(ctx context.Context) (bool, error), freq time.Duration) error {
	ticker := time.NewTicker(freq)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			stop, err := f(ctx)
			if err != nil {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					return err
				}
			}
			if stop {
				select {
				case <-ctx.Done():
					return ctx.Err()
				default:
					return nil
				}
			}
		}
	}
}

// GetAppPods fetches app pod names for a namespace.
// For Http based tests, we identify pods with format id=<pod_name>, while
// for Kafka based tests, we identify pods with the format app=<pod_name>.
func GetAppPods(apps []string, namespace string, kubectl *Kubectl, appFmt string) map[string]string {
	appPods := make(map[string]string)
	for _, v := range apps {
		res, err := kubectl.GetPodNames(namespace, fmt.Sprintf("%s=%s", appFmt, v))
		Expect(err).Should(BeNil())
		Expect(res).Should(Not(BeNil()))
		Expect(len(res)).To(BeNumerically(">", 0))
		appPods[v] = res[0]
		log.Infof("GetAppPods: pod=%q assigned to %q", res[0], v)
	}
	return appPods
}

// HoldEnvironment prints the current test status, then pauses the test
// execution. Developers who are writing tests may wish to invoke this function
// directly from test code to assist troubleshooting and test development.
func HoldEnvironment(description ...string) {
	test := ginkgo.CurrentGinkgoTestDescription()
	pid := unix.Getpid()

	fmt.Fprintf(os.Stdout, "\n---\n%s", test.FullTestText)
	fmt.Fprintf(os.Stdout, "\nat %s:%d", test.FileName, test.LineNumber)
	fmt.Fprintf(os.Stdout, "\n\n%s", description)
	fmt.Fprintf(os.Stdout, "\n\nPausing test for debug, use vagrant to access test setup.")
	fmt.Fprintf(os.Stdout, "\nRun \"kill -SIGCONT %d\" to continue.\n", pid)
	unix.Kill(pid, unix.SIGSTOP)
	time.Sleep(time.Millisecond)
	fmt.Fprintf(os.Stdout, "Test resumed.\n")
}

// Fail is a Ginkgo failure handler which raises a SIGSTOP for the test process
// when there is a failure, so that developers can debug the live environment.
// It is only triggered if the developer provides a commandline flag.
func Fail(description string, callerSkip ...int) {
	if len(callerSkip) > 0 {
		callerSkip[0]++
	} else {
		callerSkip = []int{1}
	}

	if config.CiliumTestConfig.HoldEnvironment {
		HoldEnvironment(description)
	}
	ginkgoext.Fail(description, callerSkip...)
}

// ReportDirectoryPath determines the directory path for exporting report
// commands in the case of test failure.
func ReportDirectoryPath() string {
	prefix := ""
	testName := ginkgoext.GetTestName()
	if strings.HasPrefix(strings.ToLower(testName), K8s) {
		prefix = fmt.Sprintf("%s-", strings.Replace(GetCurrentK8SEnv(), ".", "", -1))
	}
	return filepath.Join(TestResultsPath, prefix, testName)
}

// CreateReportDirectory creates and returns the directory path to export all report
// commands that need to be run in the case that a test has failed.
// If the directory cannot be created it'll return an error
func CreateReportDirectory() (string, error) {
	testPath := ReportDirectoryPath()
	if _, err := os.Stat(testPath); err == nil {
		return testPath, nil
	}
	err := os.MkdirAll(testPath, os.ModePerm)
	return testPath, err
}

// CreateLogFile creates the ReportDirectory if it is not present, writes the
// given data to the given filename.
func CreateLogFile(filename string, data []byte) error {
	path, err := CreateReportDirectory()
	if err != nil {
		log.WithError(err).Errorf("ReportDirectory cannot be created")
		return err
	}

	finalPath := filepath.Join(path, filename)
	err = ioutil.WriteFile(finalPath, data, LogPerm)
	return err
}

// WriteToReportFile writes data to filename. It appends to existing files.
func WriteToReportFile(data []byte, filename string) error {
	testPath, err := CreateReportDirectory()
	if err != nil {
		log.WithError(err).Errorf("cannot create test results path '%s'", testPath)
		return err
	}

	err = WriteOrAppendToFile(
		filepath.Join(testPath, filename),
		data,
		LogPerm)
	if err != nil {
		log.WithError(err).Errorf("cannot create monitor log file %s", filename)
		return err
	}
	return nil
}

// reportMap saves the output of the given commands to the specified filename.
// Function needs a directory path where the files are going to be written and
// a *SSHMeta instance to execute the commands
func reportMap(path string, reportCmds map[string]string, node *SSHMeta) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	reportMapContext(ctx, path, reportCmds, node)
}

// reportMap saves the output of the given commands to the specified filename.
// Function needs a directory path where the files are going to be written and
// a *SSHMeta instance to execute the commands
func reportMapContext(ctx context.Context, path string, reportCmds map[string]string, node *SSHMeta) {
	if node == nil {
		log.Errorf("cannot execute reportMap due invalid node instance")
		return
	}

	for cmd, logfile := range reportCmds {
		res := node.ExecContext(ctx, cmd, ExecOptions{SkipLog: true})
		err := ioutil.WriteFile(
			fmt.Sprintf("%s/%s", path, logfile),
			res.CombineOutput().Bytes(),
			LogPerm)
		if err != nil {
			log.WithError(err).Errorf("cannot create test results for command '%s'", cmd)
		}
	}
}

// ManifestGet returns the full path of the given manifest corresponding to the
// Kubernetes version being tested, if such a manifest exists, if not it
// returns the global manifest file.
// The paths are checked in order:
// 1- base_path/integration/filename
// 2- base_path/k8s_version/integration/filename
// 3- base_path/k8s_version/filename
// 4- base_path/filename
func ManifestGet(base, manifestFilename string) string {
	// Try dependent integration file only if we have one configured. This is
	// needed since no integration is "" and that causes us to find the
	// base_path/filename before we check the base_path/k8s_version/filename
	if integration := GetCurrentIntegration(); integration != "" {
		fullPath := filepath.Join(manifestsPath, integration, manifestFilename)
		_, err := os.Stat(fullPath)
		if err == nil {
			return filepath.Join(base, fullPath)
		}

		// try dependent k8s version and integration file
		fullPath = filepath.Join(manifestsPath, GetCurrentK8SEnv(), integration, manifestFilename)
		_, err = os.Stat(fullPath)
		if err == nil {
			return filepath.Join(base, fullPath)
		}
	}

	// try dependent k8s version
	fullPath := filepath.Join(manifestsPath, GetCurrentK8SEnv(), manifestFilename)
	_, err := os.Stat(fullPath)
	if err == nil {
		return filepath.Join(base, fullPath)
	}
	return filepath.Join(base, "k8sT", "manifests", manifestFilename)
}

// WriteOrAppendToFile writes data to a file named by filename.
// If the file does not exist, WriteFile creates it with permissions perm;
// otherwise WriteFile appends the data to the file
func WriteOrAppendToFile(filename string, data []byte, perm os.FileMode) error {
	f, err := os.OpenFile(filename, os.O_APPEND|os.O_WRONLY|os.O_CREATE, perm)
	if err != nil {
		return err
	}
	n, err := f.Write(data)
	if err == nil && n < len(data) {
		err = io.ErrShortWrite
	}
	if err1 := f.Close(); err == nil {
		err = err1
	}
	return err
}

// DNSDeployment returns the manifest to install dns engine on the server.
func DNSDeployment(base string) string {
	var DNSEngine = "coredns"
	k8sVersion := GetCurrentK8SEnv()
	switch k8sVersion {
	case "1.7", "1.8", "1.9", "1.10":
		DNSEngine = "kubedns"
	}

	if integration := GetCurrentIntegration(); integration != "" {
		fullPath := filepath.Join("provision", "manifest", k8sVersion, integration, DNSEngine+"_deployment.yaml")
		_, err := os.Stat(fullPath)
		if err == nil {
			return filepath.Join(base, fullPath)
		}
	}

	fullPath := filepath.Join("provision", "manifest", k8sVersion, DNSEngine+"_deployment.yaml")
	_, err := os.Stat(fullPath)
	if err == nil {
		return filepath.Join(base, fullPath)
	}
	return filepath.Join(base, "provision", "manifest", DNSEngine+"_deployment.yaml")
}

// getK8sSupportedConstraints returns the Kubernetes versions supported by
// a specific Cilium version.
func getK8sSupportedConstraints(ciliumVersion string) (go_version.Range, error) {
	cst, err := versioncheck.Version(ciliumVersion)
	if err != nil {
		return nil, err
	}
	switch {
	case IsCiliumV1_5(cst):
		return versioncheck.MustCompile(">=1.8.0 <1.16.0"), nil
	case IsCiliumV1_6(cst):
		return versioncheck.MustCompile(">=1.8.0 <1.18.0"), nil
	case IsCiliumV1_7(cst):
		return versioncheck.MustCompile(">=1.10.0 <1.18.0"), nil
	case IsCiliumV1_8(cst):
		return versioncheck.MustCompile(">=1.10.0 <1.19.0"), nil
	case IsCiliumV1_9(cst):
		return versioncheck.MustCompile(">=1.12.0 <1.20.0"), nil
	default:
		return nil, fmt.Errorf("unrecognized version '%s'", ciliumVersion)
	}
}

// CanRunK8sVersion returns true if the givel ciliumVersion can run in the given
// Kubernetes version. If any version is unparsable, an error is returned.
func CanRunK8sVersion(ciliumVersion, k8sVersionStr string) (bool, error) {
	k8sVersion, err := versioncheck.Version(k8sVersionStr)
	if err != nil {
		return false, err
	}
	constraint, err := getK8sSupportedConstraints(ciliumVersion)
	if err != nil {
		return false, err
	}
	return constraint(k8sVersion), nil
}

// failIfContainsBadLogMsg makes a test case to fail if any message from
// given log messages contains an entry from the blacklist (map key) AND
// does not contain ignore messages (map value).
func failIfContainsBadLogMsg(logs, label string, blacklist map[string][]string) {
	uniqueFailures := make(map[string]int)
	for _, msg := range strings.Split(logs, "\n") {
		for fail, ignoreMessages := range blacklist {
			if strings.Contains(msg, fail) {
				ok := false
				for _, ignore := range ignoreMessages {
					if strings.Contains(msg, ignore) {
						ok = true
						break
					}
				}
				if !ok {
					count, _ := uniqueFailures[fail]
					uniqueFailures[fail] = count + 1
				}
			}
		}
	}
	if len(uniqueFailures) > 0 {
		failures := make([]string, 0, len(uniqueFailures))
		for f, c := range uniqueFailures {
			failures = append(failures, f)
			fmt.Fprintf(CheckLogs, "⚠️  Found %q in logs %d times\n", f, c)
		}
		failureMsgs := strings.Join(failures, "\n")
		Fail(fmt.Sprintf("Found %d %s logs matching list of errors that must be investigated:\n%s", len(uniqueFailures), label, failureMsgs))
	}
}

// RunsOnNetNextKernel checks whether a test case is running on the net-next
// kernel (depending on the image, it's the latest kernel either from net-next.git
// or bpf-next.git tree).
func RunsOnNetNextKernel() bool {
	netNext := os.Getenv("NETNEXT")
	return netNext == "true" || netNext == "1"
}

// DoesNotRunOnNetNextKernel is the complement function of RunsOnNetNextKernel.
func DoesNotRunOnNetNextKernel() bool {
	return !RunsOnNetNextKernel()
}

// RunsOn419Kernel checks whether a test case is running on the 4.19 kernel.
func RunsOn419Kernel() bool {
	return os.Getenv("KERNEL") == "419"
}

// DoesNotRunOn419Kernel is the complement function of RunsOn419Kernel.
func DoesNotRunOn419Kernel() bool {
	return !RunsOn419Kernel()
}

// RunsOnNetNextOr419Kernel checks whether a test case is running on the net-next
// kernel (depending on the image, it's the latest kernel either from net-next.git
// or bpf-next.git tree), or on the > 4.19.57 kernel.
func RunsOnNetNextOr419Kernel() bool {
	return RunsOnNetNextKernel() || RunsOn419Kernel()
}

// DoesNotRunOnNetNextOr419Kernel is the complement function of
// RunsOnNetNextOr419Kernel.
func DoesNotRunOnNetNextOr419Kernel() bool {
	return !RunsOnNetNextOr419Kernel()
}

// DoesNotHaveHosts returns a function which returns true if a CI job
// has less VMs than the given count.
func DoesNotHaveHosts(count int) func() bool {
	return func() bool {
		if c, err := strconv.Atoi(os.Getenv("K8S_NODES")); err != nil {
			return true
		} else {
			return c < count
		}
	}
}

// RunsWithHostFirewall returns true is Cilium runs with the host firewall enabled.
func RunsWithHostFirewall() bool {
	return os.Getenv("HOST_FIREWALL") != "0" && os.Getenv("HOST_FIREWALL") != ""
}

// RunsWithKubeProxy returns true if cilium runs together with k8s' kube-proxy.
func RunsWithKubeProxy() bool {
	return os.Getenv("KUBEPROXY") != "0"
}

// RunsWithoutKubeProxy is the complement function of RunsWithKubeProxy.
func RunsWithoutKubeProxy() bool {
	return !RunsWithKubeProxy()
}

// ExistNodeWithoutCilium returns true if there is a node in a cluster which does
// not run cilium.
func ExistNodeWithoutCilium() bool {
	return GetNodeWithoutCilium() != ""
}

// DoesNotExistNodeWithoutCilium is the complement function of ExistNodeWithoutCilium
func DoesNotExistNodeWithoutCilium() bool {
	return !ExistNodeWithoutCilium()
}

func (kub *Kubectl) HasHostReachableServices(pod string, checkTCP, checkUDP bool) bool {
	status := kub.CiliumExecContext(context.TODO(), pod,
		"cilium status -o jsonpath='{.kube-proxy-replacement.features.hostReachableServices}'")
	status.ExpectSuccess("Failed to get status: %s", status.OutputPrettyPrint())
	lines := status.ByLines()
	Expect(len(lines)).ShouldNot(Equal(0), "Failed to get hostReachableServices status")

	// One-line result is e.g. "{true [TCP UDP]}" if host-reachable
	// services are activated for both protocols.
	if checkUDP && !strings.Contains(lines[0], "UDP") {
		return false
	}
	if checkTCP && !strings.Contains(lines[0], "TCP") {
		return false
	}
	return true
}

// GetNodeWithoutCilium returns a name of a node which does not run cilium.
func GetNodeWithoutCilium() string {
	return os.Getenv("NO_CILIUM_ON_NODE")
}

// GetLatestImageVersion infers which docker tag should be used
func GetLatestImageVersion() string {
	if len(config.CiliumTestConfig.CiliumTag) > 0 {
		return config.CiliumTestConfig.CiliumTag
	}
	return "latest"
}

// SkipQuarantined returns whether test under quarantine should be skipped
func SkipQuarantined() bool {
	return !config.CiliumTestConfig.RunQuarantined
}

// SkipGKEQuarantined returns whether test under quarantine on GKE should be skipped
func SkipGKEQuarantined() bool {
	return SkipQuarantined() && IsIntegration(CIIntegrationGKE)
}

// SkipRaceDetectorEnabled returns whether tests failing with race detector
// enabled should be skipped.
func SkipRaceDetectorEnabled() bool {
	race := os.Getenv("RACE")
	return race == "1" || race == "true"
}

// SkipK8sVersions returns true if the current K8s versions matched the
// constraints passed in argument.
func SkipK8sVersions(k8sVersions string) bool {
	k8sVersion, err := versioncheck.Version(GetCurrentK8SEnv())
	if err != nil {
		return false
	}
	constraint := versioncheck.MustCompile(k8sVersions)
	return constraint(k8sVersion)
}
