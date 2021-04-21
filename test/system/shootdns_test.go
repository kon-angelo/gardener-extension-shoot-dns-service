/*
 * Copyright 2021 SAP SE or an SAP affiliate company. All rights reserved. This file is licensed under the Apache Software License, v. 2 except as noted otherwise in the LICENSE file
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 *
 */

package system_test

import (
	"context"
	"flag"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/gardener/gardener-extension-shoot-dns-service/test/resources/templates"
	"github.com/gardener/gardener/extensions/pkg/controller"
	"github.com/gardener/gardener/pkg/client/kubernetes"
	"github.com/gardener/gardener/test/framework"
	"github.com/pkg/errors"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	. "github.com/onsi/ginkgo"
)

var testCfg *testConfig

type testConfig struct {
	ShootKubeconfig  string
	SeedKubeconfig   string
	ShootName        string
	ProjectNamespace string
}

func init() {
	testCfg = RegisterTestFlags()
}

func RegisterTestFlags() *testConfig {

	newCfg := &testConfig{}

	flag.StringVar(&newCfg.ShootKubeconfig, "shoot-kubecfg", "", "the path with the shoot kubeconfig.")
	flag.StringVar(&newCfg.SeedKubeconfig, "seed-kubecfg", "", "the path with the seed kubeconfig.")
	flag.StringVar(&newCfg.ShootName, "shoot-name", "", "the shoot name")
	flag.StringVar(&newCfg.ProjectNamespace, "project-namespace", "", "the project namespace of the shoot")

	return newCfg
}

type ShootDNSFramework struct {
	*framework.CommonFramework
	config testConfig
}

func NewShootDNSFramework(cfg *framework.CommonConfig) *ShootDNSFramework {
	return &ShootDNSFramework{
		CommonFramework: framework.NewCommonFramework(&framework.CommonConfig{
			ResourceDir: "../resources",
		}),
		config: *testCfg,
	}
}

func (f *ShootDNSFramework) TechnicalShootId() string {
	middle := f.config.ProjectNamespace
	if strings.HasPrefix(middle, "garden-") {
		middle = middle[7:]
	}
	return fmt.Sprintf("shoot--%s--%s", middle, f.config.ShootName)
}

func (f *ShootDNSFramework) createEchoheaders(ctx context.Context, seedClient, shootClient kubernetes.Interface, svcLB, delete bool) {
	cluster, err := controller.GetCluster(ctx, seedClient.Client(), f.TechnicalShootId())
	framework.ExpectNoError(err)
	if !cluster.Shoot.Spec.Addons.NginxIngress.Enabled {
		Fail("The test requires .spec.addons.nginxIngress.enabled to be true")
	}
	if cluster.Shoot.Spec.DNS == nil || cluster.Shoot.Spec.DNS.Domain == nil {
		Fail("The test requires .spec.dns.domain to be set")
	}

	suffix := "ingress"
	if svcLB {
		suffix = "service-lb"
	}
	namespace := fmt.Sprintf("shootdns-test-echoserver-%s", suffix)
	f.Logger.Printf("using namespace %s", namespace)
	ns := &v1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: namespace,
		},
	}

	err = shootClient.Client().Create(ctx, ns)
	framework.ExpectNoError(err)

	values := map[string]interface{}{
		"EchoName":                fmt.Sprintf("echo-%s", suffix),
		"Namespace":               namespace,
		"ShootDnsName":            *cluster.Shoot.Spec.DNS.Domain,
		"ServiceTypeLoadBalancer": svcLB,
	}
	err = f.RenderAndDeployTemplate(ctx, shootClient, templates.EchoserverApp, values)
	framework.ExpectNoError(err)

	domainName := fmt.Sprintf("%s.%s", values["EchoName"], values["ShootDnsName"])
	err = runHttpRequest(domainName, 120*time.Second)
	framework.ExpectNoError(err)

	if delete {
		f.Logger.Printf("deleting namespace %s", namespace)
		err = shootClient.Client().Delete(ctx, ns)
		framework.ExpectNoError(err)
		err = f.WaitUntilNamespaceIsDeleted(ctx, shootClient, namespace)
		framework.ExpectNoError(err)
		f.Logger.Printf("deleted namespace %s", namespace)
	} else {
		f.Logger.Printf("no cleanup of namespace %s", namespace)
	}
}

var _ = Describe("ShootDNS test", func() {

	f := NewShootDNSFramework(&framework.CommonConfig{
		ResourceDir: "../resources",
	})

	var seedClient kubernetes.Interface
	var shootClient kubernetes.Interface

	BeforeEach(func() {
		var err error
		seedClient, err = kubernetes.NewClientFromFile("", f.config.SeedKubeconfig, kubernetes.WithClientOptions(
			client.Options{
				Scheme: kubernetes.SeedScheme,
			}),
		)
		framework.ExpectNoError(err)
		shootClient, err = kubernetes.NewClientFromFile("", f.config.ShootKubeconfig, kubernetes.WithClientOptions(
			client.Options{
				Scheme: kubernetes.ShootScheme,
			}),
		)
		framework.ExpectNoError(err)
	}, 60)

	framework.CIt("Create and delete echoheaders service with type LoadBalancer", func(ctx context.Context) {
		f.createEchoheaders(ctx, seedClient, shootClient, true, true)
	}, 240*time.Second)

	framework.CIt("Create echoheaders ingress", func(ctx context.Context) {
		// cleanup during shoot deletion to test proper cleanup
		f.createEchoheaders(ctx, seedClient, shootClient, false, false)
	}, 240*time.Second)
})

func runHttpRequest(domainName string, timeout time.Duration) error {
	// first make a DNS lookup to avoid long waiting time because of negative DNS caching

	url := fmt.Sprintf("http://%s", domainName)
	var lastErr error
	end := time.Now().Add(timeout)
	for time.Now().Before(end) {
		time.Sleep(1 * time.Second)
		_, err := lookupHost(domainName, "8.8.8.8")
		if err != nil {
			lastErr = errors.Wrapf(err, "lookup host %s failed", domainName)
			continue
		}
		resp, err := http.Get(url)
		if err != nil {
			lastErr = err
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			lastErr = fmt.Errorf("unexpected status code: %d %s", resp.StatusCode, resp.Status)
			continue
		}
		return nil
	}
	return lastErr
}

func lookupHost(host, dnsServer string) ([]string, error) {
	r := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{
				Timeout: time.Millisecond * time.Duration(10000),
			}
			return d.DialContext(ctx, network, fmt.Sprintf("%s:53", dnsServer))
		},
	}
	return r.LookupHost(context.Background(), host)
}
