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

package e2e

import (
	"fmt"
	"strconv"
	"time"

	"k8s.io/kubernetes/pkg/api"
	"k8s.io/kubernetes/pkg/client/cache"
	client "k8s.io/kubernetes/pkg/client/unversioned"
	"k8s.io/kubernetes/pkg/labels"
	"k8s.io/kubernetes/pkg/master/ports"
	"k8s.io/kubernetes/pkg/runtime"
	"k8s.io/kubernetes/pkg/util/sets"
	"k8s.io/kubernetes/pkg/util/uuid"
	"k8s.io/kubernetes/pkg/util/wait"
	"k8s.io/kubernetes/pkg/watch"
	"k8s.io/kubernetes/test/e2e/framework"
	testutils "k8s.io/kubernetes/test/utils"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
)

// This test primarily checks 2 things:
// 1. Daemons restart automatically within some sane time (10m).
// 2. They don't take abnormal actions when restarted in the steady state.
//	- Controller manager shouldn't overshoot replicas
//	- Kubelet shouldn't restart containers
//	- Scheduler should continue assigning hosts to new pods

const (
	restartPollInterval = 5 * time.Second
	restartTimeout      = 10 * time.Minute
	numPods             = 10
	sshPort             = 22
	ADD                 = "ADD"
	DEL                 = "DEL"
	UPDATE              = "UPDATE"
)

// nodeExec execs the given cmd on node via SSH. Note that the nodeName is an sshable name,
// eg: the name returned by framework.GetMasterHost(). This is also not guaranteed to work across
// cloud providers since it involves ssh.
func nodeExec(nodeName, cmd string) (framework.SSHResult, error) {
	result, err := framework.SSH(cmd, fmt.Sprintf("%v:%v", nodeName, sshPort), framework.TestContext.Provider)
	Expect(err).NotTo(HaveOccurred())
	return result, err
}

// restartDaemonConfig is a config to restart a running daemon on a node, and wait till
// it comes back up. It uses ssh to send a SIGTERM to the daemon.
type restartDaemonConfig struct {
	nodeName     string
	daemonName   string
	healthzPort  int
	pollInterval time.Duration
	pollTimeout  time.Duration
}

// NewRestartConfig creates a restartDaemonConfig for the given node and daemon.
func NewRestartConfig(nodeName, daemonName string, healthzPort int, pollInterval, pollTimeout time.Duration) *restartDaemonConfig {
	if !framework.ProviderIs("gce") {
		framework.Logf("WARNING: SSH through the restart config might not work on %s", framework.TestContext.Provider)
	}
	return &restartDaemonConfig{
		nodeName:     nodeName,
		daemonName:   daemonName,
		healthzPort:  healthzPort,
		pollInterval: pollInterval,
		pollTimeout:  pollTimeout,
	}
}

func (r *restartDaemonConfig) String() string {
	return fmt.Sprintf("Daemon %v on node %v", r.daemonName, r.nodeName)
}

// waitUp polls healthz of the daemon till it returns "ok" or the polling hits the pollTimeout
func (r *restartDaemonConfig) waitUp() {
	framework.Logf("Checking if %v is up by polling for a 200 on its /healthz endpoint", r)
	healthzCheck := fmt.Sprintf(
		"curl -s -o /dev/null -I -w \"%%{http_code}\" http://localhost:%v/healthz", r.healthzPort)

	err := wait.Poll(r.pollInterval, r.pollTimeout, func() (bool, error) {
		result, err := nodeExec(r.nodeName, healthzCheck)
		framework.ExpectNoError(err)
		if result.Code == 0 {
			httpCode, err := strconv.Atoi(result.Stdout)
			if err != nil {
				framework.Logf("Unable to parse healthz http return code: %v", err)
			} else if httpCode == 200 {
				return true, nil
			}
		}
		framework.Logf("node %v exec command, '%v' failed with exitcode %v: \n\tstdout: %v\n\tstderr: %v",
			r.nodeName, healthzCheck, result.Code, result.Stdout, result.Stderr)
		return false, nil
	})
	framework.ExpectNoError(err, "%v did not respond with a 200 via %v within %v", r, healthzCheck, r.pollTimeout)
}

// kill sends a SIGTERM to the daemon
func (r *restartDaemonConfig) kill() {
	framework.Logf("Killing %v", r)
	nodeExec(r.nodeName, fmt.Sprintf("pgrep %v | xargs -I {} sudo kill {}", r.daemonName))
}

// Restart checks if the daemon is up, kills it, and waits till it comes back up
func (r *restartDaemonConfig) restart() {
	r.waitUp()
	r.kill()
	r.waitUp()
}

// podTracker records a serial history of events that might've affects pods.
type podTracker struct {
	cache.ThreadSafeStore
}

func (p *podTracker) remember(pod *api.Pod, eventType string) {
	if eventType == UPDATE && pod.Status.Phase == api.PodRunning {
		return
	}
	p.Add(fmt.Sprintf("[%v] %v: %v", time.Now(), eventType, pod.Name), pod)
}

func (p *podTracker) String() (msg string) {
	for _, k := range p.ListKeys() {
		obj, exists := p.Get(k)
		if !exists {
			continue
		}
		pod := obj.(*api.Pod)
		msg += fmt.Sprintf("%v Phase %v Host %v\n", k, pod.Status.Phase, pod.Spec.NodeName)
	}
	return
}

func newPodTracker() *podTracker {
	return &podTracker{cache.NewThreadSafeStore(
		cache.Indexers{}, cache.Indices{})}
}

// replacePods replaces content of the store with the given pods.
func replacePods(pods []*api.Pod, store cache.Store) {
	found := make([]interface{}, 0, len(pods))
	for i := range pods {
		found = append(found, pods[i])
	}
	framework.ExpectNoError(store.Replace(found, "0"))
}

// getContainerRestarts returns the count of container restarts across all pods matching the given labelSelector,
// and a list of nodenames across which these containers restarted.
func getContainerRestarts(c *client.Client, ns string, labelSelector labels.Selector) (int, []string) {
	options := api.ListOptions{LabelSelector: labelSelector}
	pods, err := c.Pods(ns).List(options)
	framework.ExpectNoError(err)
	failedContainers := 0
	containerRestartNodes := sets.NewString()
	for _, p := range pods.Items {
		for _, v := range testutils.FailedContainers(&p) {
			failedContainers = failedContainers + v.Restarts
			containerRestartNodes.Insert(p.Spec.NodeName)
		}
	}
	return failedContainers, containerRestartNodes.List()
}

var _ = framework.KubeDescribe("DaemonRestart [Disruptive]", func() {

	f := framework.NewDefaultFramework("daemonrestart")
	rcName := "daemonrestart" + strconv.Itoa(numPods) + "-" + string(uuid.NewUUID())
	labelSelector := labels.Set(map[string]string{"name": rcName}).AsSelector()
	existingPods := cache.NewStore(cache.MetaNamespaceKeyFunc)
	var ns string
	var config testutils.RCConfig
	var controller *cache.Controller
	var newPods cache.Store
	var stopCh chan struct{}
	var tracker *podTracker

	BeforeEach(func() {
		// These tests require SSH
		framework.SkipUnlessProviderIs(framework.ProvidersWithSSH...)
		ns = f.Namespace.Name

		// All the restart tests need an rc and a watch on pods of the rc.
		// Additionally some of them might scale the rc during the test.
		config = testutils.RCConfig{
			Client:      f.Client,
			Name:        rcName,
			Namespace:   ns,
			Image:       framework.GetPauseImageName(f.Client),
			Replicas:    numPods,
			CreatedPods: &[]*api.Pod{},
		}
		Expect(framework.RunRC(config)).NotTo(HaveOccurred())
		replacePods(*config.CreatedPods, existingPods)

		stopCh = make(chan struct{})
		tracker = newPodTracker()
		newPods, controller = cache.NewInformer(
			&cache.ListWatch{
				ListFunc: func(options api.ListOptions) (runtime.Object, error) {
					options.LabelSelector = labelSelector
					return f.Client.Pods(ns).List(options)
				},
				WatchFunc: func(options api.ListOptions) (watch.Interface, error) {
					options.LabelSelector = labelSelector
					return f.Client.Pods(ns).Watch(options)
				},
			},
			&api.Pod{},
			0,
			cache.ResourceEventHandlerFuncs{
				AddFunc: func(obj interface{}) {
					tracker.remember(obj.(*api.Pod), ADD)
				},
				UpdateFunc: func(oldObj, newObj interface{}) {
					tracker.remember(newObj.(*api.Pod), UPDATE)
				},
				DeleteFunc: func(obj interface{}) {
					tracker.remember(obj.(*api.Pod), DEL)
				},
			},
		)
		go controller.Run(stopCh)
	})

	AfterEach(func() {
		close(stopCh)
	})

	It("Controller Manager should not create/delete replicas across restart", func() {

		// Requires master ssh access.
		framework.SkipUnlessProviderIs("gce", "aws")
		restarter := NewRestartConfig(
			framework.GetMasterHost(), "kube-controller", ports.ControllerManagerPort, restartPollInterval, restartTimeout)
		restarter.restart()

		// The intent is to ensure the replication controller manager has observed and reported status of
		// the replication controller at least once since the manager restarted, so that we can determine
		// that it had the opportunity to create/delete pods, if it were going to do so. Scaling the RC
		// to the same size achieves this, because the scale operation advances the RC's sequence number
		// and awaits it to be observed and reported back in the RC's status.
		framework.ScaleRC(f.Client, f.ClientSet, ns, rcName, numPods, true)

		// Only check the keys, the pods can be different if the kubelet updated it.
		// TODO: Can it really?
		existingKeys := sets.NewString()
		newKeys := sets.NewString()
		for _, k := range existingPods.ListKeys() {
			existingKeys.Insert(k)
		}
		for _, k := range newPods.ListKeys() {
			newKeys.Insert(k)
		}
		if len(newKeys.List()) != len(existingKeys.List()) ||
			!newKeys.IsSuperset(existingKeys) {
			framework.Failf("RcManager created/deleted pods after restart \n\n %+v", tracker)
		}
	})

	It("Scheduler should continue assigning pods to nodes across restart", func() {

		// Requires master ssh access.
		framework.SkipUnlessProviderIs("gce", "aws")
		restarter := NewRestartConfig(
			framework.GetMasterHost(), "kube-scheduler", ports.SchedulerPort, restartPollInterval, restartTimeout)

		// Create pods while the scheduler is down and make sure the scheduler picks them up by
		// scaling the rc to the same size.
		restarter.waitUp()
		restarter.kill()
		// This is best effort to try and create pods while the scheduler is down,
		// since we don't know exactly when it is restarted after the kill signal.
		framework.ExpectNoError(framework.ScaleRC(f.Client, f.ClientSet, ns, rcName, numPods+5, false))
		restarter.waitUp()
		framework.ExpectNoError(framework.ScaleRC(f.Client, f.ClientSet, ns, rcName, numPods+5, true))
	})

	It("Kubelet should not restart containers across restart", func() {

		nodeIPs, err := getNodePublicIps(f.ClientSet)
		framework.ExpectNoError(err)
		preRestarts, badNodes := getContainerRestarts(f.Client, ns, labelSelector)
		if preRestarts != 0 {
			framework.Logf("WARNING: Non-zero container restart count: %d across nodes %v", preRestarts, badNodes)
		}
		for _, ip := range nodeIPs {
			restarter := NewRestartConfig(
				ip, "kubelet", ports.KubeletReadOnlyPort, restartPollInterval, restartTimeout)
			restarter.restart()
		}
		postRestarts, badNodes := getContainerRestarts(f.Client, ns, labelSelector)
		if postRestarts != preRestarts {
			framework.DumpNodeDebugInfo(f.Client, badNodes, framework.Logf)
			framework.Failf("Net container restart count went from %v -> %v after kubelet restart on nodes %v \n\n %+v", preRestarts, postRestarts, badNodes, tracker)
		}
	})
})
