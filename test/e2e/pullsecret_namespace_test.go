// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootcv1alpha1 "github.com/bootc-dev/bootc-operator/api/v1alpha1"
	"github.com/bootc-dev/bootc-operator/test/e2e/e2eutil"
	testutil "github.com/bootc-dev/bootc-operator/test/util"
)

// TestPullSecretNamespaces verifies that pools can only use pull secrets
// from the namespaces the operator is configured to read from. The
// operator must already allow E2E_PULL_SECRET_NAMESPACE (make deploy-bink
// does). A pool referencing a secret in a namespace the test creates must
// be Degraded without claiming its node; switched to a secret in the
// allowed namespace, its node must update through the authenticated
// registry.
func TestPullSecretNamespaces(t *testing.T) {
	e2eutil.Providers(t, "bink", "eks")
	t.Parallel()
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)

	env := e2eutil.New(t)
	if env.RegistryUser() == "" || env.RegistryPassword() == "" {
		t.Skip("E2E_REGISTRY_USER / E2E_REGISTRY_PASSWORD not set")
	}
	allowedNS := os.Getenv("E2E_PULL_SECRET_NAMESPACE")
	if allowedNS == "" {
		t.Skip("E2E_PULL_SECRET_NAMESPACE not set")
	}

	ctx := context.Background()
	configuredNamespaces := controllerPullSecretNamespaces(t, g, ctx, env.Client)
	if !slices.Contains(configuredNamespaces, allowedNS) {
		t.Fatalf(
			"operator must allow pull secrets from %q; configured namespaces: %v",
			allowedNS,
			configuredNamespaces,
		)
	}

	deniedNS := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: env.TestID() + "-denied"}}
	g.Expect(env.Client.Create(ctx, deniedNS)).To(Succeed())
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), deniedNS) })

	nodeName := env.AddNode(t)

	deniedSecret := registrySecret(env, env.TestID()+"-pull-secret", deniedNS.Name)
	g.Expect(env.Client.Create(ctx, deniedSecret)).To(Succeed())
	allowedSecret := registrySecret(env, env.TestID()+"-pull-secret", allowedNS)
	g.Expect(env.Client.Create(ctx, allowedSecret)).To(Succeed())
	t.Cleanup(func() { _ = env.Client.Delete(context.Background(), allowedSecret) })

	authImageRef := env.AuthImageRef()
	pool := env.NewPool("psns", authImageRef,
		testutil.WithPullSecret(deniedSecret.Name, deniedSecret.Namespace),
	)
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())

	// Phase 1: the secret exists, but its namespace is not allowed.
	g.Eventually(func() ([]metav1.Condition, error) {
		var p bootcv1alpha1.BootcNodePool
		err := env.Client.Get(ctx, client.ObjectKeyFromObject(pool), &p)
		return p.Status.Conditions, err
	}).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolSecretError),
		HaveField("Message", ContainSubstring(fmt.Sprintf("%q", deniedNS.Name))),
	)))
	g.Consistently(func() bool {
		var bn bootcv1alpha1.BootcNode
		err := env.Client.Get(ctx, client.ObjectKey{Name: nodeName}, &bn)
		return apierrors.IsNotFound(err)
	}).WithTimeout(10*time.Second).Should(BeTrue(),
		"no BootcNode should be created for a pool with a denied secret")

	t.Logf("Pool %q is Degraded for secret in namespace %q", pool.Name, deniedNS.Name)

	// Phase 2: a secret in the allowed namespace is used to pull the image.
	modified := pool.DeepCopy()
	modified.Spec.PullSecretRef = &bootcv1alpha1.PullSecretRef{
		Name: allowedSecret.Name, Namespace: allowedSecret.Namespace,
	}
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	*pool = *modified

	testutil.WaitForNodeIdle(t, g, ctx, env.Client, nodeName, 8*time.Minute,
		HaveField("Booted", HaveField("Image", Equal(authImageRef))),
	)
	g.Eventually(fetchPoolStatus(ctx, env.Client, pool)).
		Should(poolAllUpdated(1, env.NodeImageUpdateDigest()))

	t.Logf("Node %q updated with a pull secret from namespace %q", nodeName, allowedNS)
}

// TestOperatorConfigPullSecretNamespaces changes shared startup configuration,
// so it must stay serial. No node is provisioned: reconciling a digest-based pool
// proves that the controller can read a Secret through its namespace-scoped cache.
func TestOperatorConfigPullSecretNamespaces(t *testing.T) {
	e2eutil.Providers(t, "bink")
	g := NewWithT(t)
	g.SetDefaultEventuallyTimeout(pollTimeout)
	g.SetDefaultEventuallyPollingInterval(pollInterval)
	env := e2eutil.New(t)
	ctx := context.Background()
	var originalDeploy appsv1.Deployment
	g.Expect(env.Client.Get(ctx, operatorDeployKey(), &originalDeploy)).To(Succeed())
	originalConfig, err := readOperatorConfig(ctx, env.Client)
	g.Expect(err).NotTo(HaveOccurred())

	namespaces := []string{env.TestID() + "-allowed", env.TestID() + "-denied"}
	for _, name := range namespaces {
		ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}
		g.Expect(env.Client.Create(ctx, ns)).To(Succeed())
		t.Cleanup(
			func() { g.Expect(client.IgnoreNotFound(env.Client.Delete(ctx, ns))).To(Succeed()) },
		)
		secret := &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: "pull-secret", Namespace: name},
			Type:       corev1.SecretTypeDockerConfigJson,
			Data:       map[string][]byte{corev1.DockerConfigJsonKey: []byte(`{"auths":{}}`)},
		}
		g.Expect(env.Client.Create(ctx, secret)).To(Succeed())
	}
	role := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: "pull-secrets", Namespace: namespaces[0]},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{""},
				Resources: []string{"secrets"},
				Verbs:     []string{"get", "list", "watch"},
			},
		},
	}
	g.Expect(env.Client.Create(ctx, role)).To(Succeed())
	binding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: role.Name, Namespace: role.Namespace},
		RoleRef:    rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: role.Name},
		Subjects: []rbacv1.Subject{
			{
				Kind:      "ServiceAccount",
				Name:      originalDeploy.Spec.Template.Spec.ServiceAccountName,
				Namespace: originalDeploy.Namespace,
			},
		},
	}
	g.Expect(env.Client.Create(ctx, binding)).To(Succeed())

	var originalArgs []string
	for _, cont := range originalDeploy.Spec.Template.Spec.Containers {
		if cont.Name == "manager" {
			originalArgs = cont.Args
		}
	}
	t.Cleanup(func() {
		g.Expect(restoreOperatorConfig(ctx, env.Client, originalConfig)).To(Succeed())
		setControllerArgs(t, g, ctx, env.Client, originalArgs)
		restartOperator(t)
		waitForOperatorReady(t, g, ctx, env.Client)
	})
	cfg := originalConfig.DeepCopy()
	if cfg == nil {
		cfg = &bootcv1alpha1.BootcOperatorConfig{ObjectMeta: metav1.ObjectMeta{Name: env.TestID()}}
	}
	if cfg.Spec.Controller == nil {
		cfg.Spec.Controller = &bootcv1alpha1.OperatorControllerConfig{}
	}
	cfg.Spec.Controller.PullSecretNamespaces = []string{namespaces[0]}
	g.Expect(restoreOperatorConfig(ctx, env.Client, cfg)).To(Succeed())
	args, _ := splitNamespaceArgs(originalArgs)
	setControllerArgs(t, g, ctx, env.Client, args)
	restartOperator(t)
	waitForOperatorReady(t, g, ctx, env.Client)

	pool := env.NewPool("config-secret", env.NodeImageDigestedPullSpec(),
		testutil.WithPullSecret("pull-secret", namespaces[1]))
	g.Expect(env.Client.Create(ctx, pool)).To(Succeed())
	t.Cleanup(func() {
		g.Expect(client.IgnoreNotFound(env.Client.Delete(ctx, pool))).To(Succeed())
		g.Eventually(func() error { return env.Client.Get(ctx, client.ObjectKeyFromObject(pool), pool) }).
			Should(MatchError(apierrors.IsNotFound, "IsNotFound"))
	})
	conditions := func() ([]metav1.Condition, error) {
		var got bootcv1alpha1.BootcNodePool
		err := env.Client.Get(ctx, client.ObjectKeyFromObject(pool), &got)
		return got.Status.Conditions, err
	}
	g.Eventually(conditions).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionTrue),
		HaveField("Reason", bootcv1alpha1.PoolSecretError),
		HaveField("Message", ContainSubstring(namespaces[1])),
	)))
	modified := pool.DeepCopy()
	modified.Spec.PullSecretRef.Namespace = namespaces[0]
	g.Expect(env.Client.Patch(ctx, modified, client.MergeFrom(pool))).To(Succeed())
	g.Eventually(conditions).Should(ContainElement(And(
		HaveField("Type", bootcv1alpha1.PoolDegraded),
		HaveField("Status", metav1.ConditionFalse),
		HaveField("Reason", bootcv1alpha1.PoolHealthy),
	)))
}

func setControllerArgs(
	t *testing.T,
	g Gomega,
	ctx context.Context,
	c client.Client,
	args []string,
) {
	t.Helper()
	var d appsv1.Deployment
	g.Expect(c.Get(ctx, operatorDeployKey(), &d)).To(Succeed())
	original := d.DeepCopy()
	for i := range d.Spec.Template.Spec.Containers {
		if d.Spec.Template.Spec.Containers[i].Name == "manager" {
			d.Spec.Template.Spec.Containers[i].Args = args
			g.Expect(c.Patch(ctx, &d, client.MergeFrom(original))).To(Succeed())
			return
		}
	}
	t.Fatal("manager container not found")
}

// registrySecret returns a dockerconfigjson Secret with the credentials of
// the authenticated e2e registry.
func registrySecret(env *e2eutil.Env, name, namespace string) *corev1.Secret {
	auth := base64.StdEncoding.EncodeToString(
		[]byte(env.RegistryUser() + ":" + env.RegistryPassword()),
	)
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Type:       corev1.SecretTypeDockerConfigJson,
		Data: map[string][]byte{
			corev1.DockerConfigJsonKey: fmt.Appendf(nil,
				`{"auths":{%q:{"auth":%q}}}`, env.RegistryHost(), auth),
		},
	}
}
