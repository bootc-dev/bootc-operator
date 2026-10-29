// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

const (
	metricsServiceName = "bootc-operator-metrics"
	metricsReaderRole  = "bootc-operator-metrics-reader"
)

// TestMetricsEndpoint creates a pool, then scrapes the operator metrics
// endpoint from inside the cluster (the same way Prometheus would) and
// verifies that the expected pool and node metrics are present.
func TestMetricsEndpoint(t *testing.T) {
	e2eutil.Providers(t, "bink")
	t.Parallel()
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	nodeName := env.AddNode(t)

	ctx := context.Background()

	pool := env.NewPool("metrics", env.NodeImageDigestedPullSpec())
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 3*time.Minute,
		HaveField("Booted", imageMatchesDigest(env.NodeImageDigest())),
	)

	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageDigest()))

	t.Logf("Node %q is Idle; scraping metrics endpoint", nodeName)

	sa, crb := createMetricsScraper(t, g, ctx, env.Client, env.TestID())
	t.Cleanup(func() {
		_ = env.Client.Delete(ctx, crb)
		_ = env.Client.Delete(ctx, sa)
	})

	metricsOutput := scrapeMetrics(t, g, ctx, env.TestID(), sa.Name)
	t.Logf("Scraped %d bytes from metrics endpoint", len(metricsOutput))

	poolName := pool.Name
	expectedMetrics := []string{
		fmt.Sprintf(`bootcoperator_pool_nodes_total{pool="%s"}`, poolName),
		fmt.Sprintf(`bootcoperator_pool_nodes_updated{pool="%s"}`, poolName),
		fmt.Sprintf(`bootcoperator_pool_nodes_updating{pool="%s"}`, poolName),
		fmt.Sprintf(`bootcoperator_pool_nodes_degraded{pool="%s"}`, poolName),
		fmt.Sprintf(`bootcoperator_pool_rollout_paused{pool="%s"}`, poolName),
		"bootcoperator_node_booted_image_info{",
	}
	for _, metric := range expectedMetrics {
		g.Expect(metricsOutput).To(ContainSubstring(metric),
			"expected metric %q in output", metric)
	}

	var bn bootcv1alpha1.BootcNode
	g.Expect(env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)).To(Succeed())
	g.Expect(metricsOutput).To(ContainSubstring(
		fmt.Sprintf(`node="%s"`, nodeName)),
		"expected node label in bootcoperator_node_booted_image_info",
	)
}

// createMetricsScraper creates a ServiceAccount and ClusterRoleBinding
// that grants it the metrics-reader ClusterRole.
func createMetricsScraper(
	t *testing.T,
	g Gomega,
	ctx context.Context,
	c client.Client,
	testID string,
) (*corev1.ServiceAccount, *rbacv1.ClusterRoleBinding) {
	t.Helper()

	saName := testID + "-metrics-scraper"

	sa := &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name:      saName,
			Namespace: testutil.OperatorNamespaceName,
		},
	}
	g.Expect(c.Create(ctx, sa)).To(Succeed())

	crb := &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name: saName,
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "ClusterRole",
			Name:     metricsReaderRole,
		},
		Subjects: []rbacv1.Subject{{
			Kind:      rbacv1.ServiceAccountKind,
			Name:      saName,
			Namespace: testutil.OperatorNamespaceName,
		}},
	}
	g.Expect(c.Create(ctx, crb)).To(Succeed())

	return sa, crb
}

// scrapeMetrics runs a curl pod with the scraper SA to hit the metrics
// endpoint from inside the cluster, the same way Prometheus would.
func scrapeMetrics(
	t *testing.T,
	g Gomega,
	ctx context.Context,
	testID string,
	saName string,
) string {
	t.Helper()

	kubeconfigPath := os.Getenv("KUBECONFIG")
	metricsURL := fmt.Sprintf(
		"https://%s.%s.svc:%d/metrics",
		metricsServiceName, testutil.OperatorNamespaceName, 8443,
	)

	curlImage := "docker.io/curlimages/curl:latest"
	podName := testID + "-curl"
	args := []string{
		"--kubeconfig", kubeconfigPath,
		"-n", testutil.OperatorNamespaceName,
		"run", podName,
		"--image=" + curlImage,
		"--restart=Never",
		"--overrides", fmt.Sprintf(`{
			"spec": {
				"serviceAccountName": "%s",
				"nodeSelector": {
					"node-role.kubernetes.io/control-plane": ""
				},
				"tolerations": [{
					"key": "node-role.kubernetes.io/control-plane",
					"operator": "Exists",
					"effect": "NoSchedule"
				}],
				"containers": [{
					"name": "curl",
					"image": "%s",
					"command": ["sh", "-c",
						"curl -sf -k -H \"Authorization: Bearer $(cat /var/run/secrets/kubernetes.io/serviceaccount/token)\" %s"
					]
				}]
			}
		}`, saName, curlImage, metricsURL),
	}

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	out, err := cmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(),
		"failed to create curl pod: %s", string(out))

	t.Cleanup(func() {
		del := exec.CommandContext(ctx, "kubectl",
			"--kubeconfig", kubeconfigPath,
			"-n", testutil.OperatorNamespaceName,
			"delete", "pod", podName, "--ignore-not-found",
		)
		_ = del.Run()
	})

	g.Eventually(func() (corev1.PodPhase, error) {
		getCmd := exec.CommandContext(ctx, "kubectl",
			"--kubeconfig", kubeconfigPath,
			"-n", testutil.OperatorNamespaceName,
			"get", "pod", podName,
			"-o", "jsonpath={.status.phase}",
		)
		out, err := getCmd.CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("kubectl get pod: %s: %w", string(out), err)
		}
		return corev1.PodPhase(strings.TrimSpace(string(out))), nil
	}).WithTimeout(2*time.Minute).WithPolling(3*time.Second).Should(
		Equal(corev1.PodSucceeded),
		"curl pod should complete successfully",
	)

	logCmd := exec.CommandContext(ctx, "kubectl",
		"--kubeconfig", kubeconfigPath,
		"-n", testutil.OperatorNamespaceName,
		"logs", podName,
	)
	logOut, err := logCmd.CombinedOutput()
	g.Expect(err).NotTo(HaveOccurred(),
		"failed to read curl pod logs: %s", string(logOut))

	return strings.TrimSpace(string(logOut))
}
