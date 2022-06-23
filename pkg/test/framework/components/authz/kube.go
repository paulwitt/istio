// Copyright Istio Authors
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

package authz

import (
	"fmt"
	"io/ioutil"
	"strings"
	"time"

	"github.com/hashicorp/go-multierror"

	meshconfig "istio.io/api/mesh/v1alpha1"
	"istio.io/istio/pkg/config/protocol"
	"istio.io/istio/pkg/test/env"
	"istio.io/istio/pkg/test/framework/components/echo"
	"istio.io/istio/pkg/test/framework/components/istio"
	"istio.io/istio/pkg/test/framework/components/namespace"
	"istio.io/istio/pkg/test/framework/resource"
	"istio.io/istio/pkg/test/framework/resource/config/apply"
	"istio.io/istio/pkg/test/framework/resource/config/cleanup"
	"istio.io/istio/pkg/test/kube"
	"istio.io/istio/pkg/test/scopes"
	"istio.io/istio/pkg/test/util/tmpl"
	"istio.io/istio/pkg/util/protomarshal"
)

const (
	httpName = "ext-authz-http"
	grpcName = "ext-authz-grpc"
	httpPort = 8000
	grpcPort = 9000

	providerTemplate = `
extensionProviders:
- name: "{{ .httpName }}"
  envoyExtAuthzHttp:
    service: "{{ .fqdn }}"
    port: {{ .httpPort }}
    headersToUpstreamOnAllow: ["x-ext-authz-*"]
    headersToDownstreamOnDeny: ["x-ext-authz-*"]
    includeRequestHeadersInCheck: ["x-ext-authz"]
    includeAdditionalHeadersInCheck:
      x-ext-authz-additional-header-new: additional-header-new-value
      x-ext-authz-additional-header-override: additional-header-override-value
- name: "{{ .grpcName }}"
  envoyExtAuthzGrpc:
    service: "{{ .fqdn }}"
    port: {{ .grpcPort }}`
)

var _ resource.Resource = &serverImpl{}

func newKubeServer(ctx resource.Context, ns namespace.Instance) (server *serverImpl, err error) {
	start := time.Now()
	scopes.Framework.Info("=== BEGIN: Deploy authz server ===")
	defer func() {
		if err != nil {
			scopes.Framework.Error("=== FAILED: Deploy authz server ===")
			scopes.Framework.Error(err)
		} else {
			scopes.Framework.Infof("=== SUCCEEDED: Deploy authz server in %v ===", time.Since(start))
		}
	}()

	// Create the namespace, if unspecified.
	if ns == nil {
		ns, err = namespace.New(ctx, namespace.Config{
			Prefix: "authz",
			Inject: true,
		})
		if err != nil {
			return
		}
	}

	server = &serverImpl{
		ns: ns,
		providers: []Provider{
			&providerImpl{
				name: httpName,
				api:  HTTP,
				protocolSupported: func(p protocol.Instance) bool {
					// HTTP protocol doesn't support raw TCP requests.
					return !p.IsTCP()
				},
				targetSupported: func(echo.Target) bool {
					return true
				},
				check: checkHTTP,
			},
			&providerImpl{
				name: grpcName,
				api:  GRPC,
				protocolSupported: func(protocol.Instance) bool {
					return true
				},
				targetSupported: func(echo.Target) bool {
					return true
				},
				check: checkGRPC,
			},
		},
	}
	server.id = ctx.TrackResource(server)

	// Deploy the authz server.
	if err = server.deploy(ctx); err != nil {
		return
	}

	// Patch MeshConfig to intall the providers.
	err = server.installProviders(ctx)
	return
}

func readDeploymentYAML(ctx resource.Context) (string, error) {
	// Read the samples file.
	filePath := fmt.Sprintf("%s/samples/extauthz/ext-authz.yaml", env.IstioSrc)
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return "", err
	}
	yamlText := string(data)

	// Replace the image.
	s := ctx.Settings().Image
	oldImage := "gcr.io/istio-testing/ext-authz:latest"
	newImage := fmt.Sprintf("%s/ext-authz:%s", s.Hub, strings.TrimSuffix(s.Tag, "-distroless"))
	yamlText = strings.ReplaceAll(yamlText, oldImage, newImage)

	// Replace the image pull policy
	oldPolicy := "IfNotPresent"
	newPolicy := s.PullPolicy
	yamlText = strings.ReplaceAll(yamlText, oldPolicy, newPolicy)

	return yamlText, nil
}

func (s *serverImpl) deploy(ctx resource.Context) error {
	yamlText, err := readDeploymentYAML(ctx)
	if err != nil {
		return err
	}

	if err := ctx.ConfigKube(ctx.Clusters()...).
		YAML(s.ns.Name(), yamlText).
		Apply(apply.CleanupConditionally); err != nil {
		return err
	}

	// Wait for the endpoints to be ready.
	var g multierror.Group
	for _, c := range ctx.Clusters() {
		c := c
		g.Go(func() error {
			_, _, err := kube.WaitUntilServiceEndpointsAreReady(c.Kube(), s.ns.Name(), "ext-authz")
			return err
		})
	}

	return g.Wait().ErrorOrNil()
}

func (s *serverImpl) installProviders(ctx resource.Context) error {
	// Update the mesh config extension provider for the ext-authz service.
	providerYAML, err := tmpl.Evaluate(providerTemplate, s.templateArgs())
	if err != nil {
		return err
	}

	return installProviders(ctx, providerYAML)
}

type serverImpl struct {
	id        resource.ID
	ns        namespace.Instance
	providers []Provider
}

func (s *serverImpl) ID() resource.ID {
	return s.id
}

func (s *serverImpl) Namespace() namespace.Instance {
	return s.ns
}

func (s *serverImpl) Providers() []Provider {
	return append([]Provider{}, s.providers...)
}

func (s *serverImpl) templateArgs() map[string]interface{} {
	fqdn := fmt.Sprintf("ext-authz.%s.svc.cluster.local", s.ns.Name())
	return map[string]interface{}{
		"fqdn":     fqdn,
		"httpName": httpName,
		"grpcName": grpcName,
		"httpPort": httpPort,
		"grpcPort": grpcPort,
	}
}

func installProviders(ctx resource.Context, providerYAML string) error {
	var ist istio.Instance
	ist, err := istio.Get(ctx)
	if err != nil {
		return err
	}

	// Now parse the provider YAML.
	newMC := &meshconfig.MeshConfig{}
	if err := protomarshal.ApplyYAML(providerYAML, newMC); err != nil {
		return err
	}

	return istio.UpdateMeshConfig(ctx, ist.Settings().SystemNamespace, ctx.Clusters(),
		func(mc *meshconfig.MeshConfig) error {
			// Merge the extension providers.
			mc.ExtensionProviders = append(mc.ExtensionProviders, newMC.ExtensionProviders...)
			return nil
		}, cleanup.Conditionally)
}
