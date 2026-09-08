/*
Copyright 2022 The KubeSphere Authors.

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

package git

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	goscm "github.com/jenkins-x/go-scm/scm"
	"github.com/jenkins-x/go-scm/scm/driver/bitbucket"
	"github.com/jenkins-x/go-scm/scm/factory"
	"github.com/jenkins-x/go-scm/scm/transport"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"kubesphere.io/devops/pkg/api/devops/v1alpha3"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// ClientFactory responsible for creating a git client
type ClientFactory struct {
	provider  string
	secretRef *v1.SecretReference
	k8sClient ResourceGetter

	Server string
}

// NewClientFactory creates an instance of the ClientFactory
func NewClientFactory(provider string, secretRef *v1.SecretReference, k8sClient ResourceGetter) *ClientFactory {
	return &ClientFactory{
		provider:  provider,
		secretRef: secretRef,
		k8sClient: k8sClient,
	}
}

// bitbucketTokenAuthUsername is the special username placeholder used when
// authenticating Bitbucket Cloud with an HTTP Access Token (Bearer token).
// Jenkins and other tools use this convention: the username is set to this
// constant and the password field holds the actual token value.
const bitbucketTokenAuthUsername = "x-bitbucket-api-token-auth"

// GetClient returns the git client with auth
func (c *ClientFactory) GetClient() (client *goscm.Client, err error) {
	provider := c.provider
	switch c.provider {
	case "bitbucket_cloud":
		provider = "bitbucketcloud"
	case "bitbucket-server":
		provider = "bitbucketserver"
	}

	if c.Server == "https://api.bitbucket.org" || c.Server == "https://bitbucket.org" {
		provider = "bitbucketcloud"
	}

	var token string
	username := ""
	if c.secretRef != nil {
		if token, username, err = c.getTokenFromSecret(c.secretRef); err != nil {
			return
		}
	}

	// Bitbucket Cloud rejects Basic Auth for HTTP Access Tokens unless the
	// username is a registered Atlassian email. When Jenkins-style credentials
	// are used (username = "x-bitbucket-api-token-auth", password = token),
	// we bypass go-scm factory and send the token as a Bearer token instead.
	// This keeps the credential format consistent with Jenkins SCM usage.
	if provider == "bitbucketcloud" && username == bitbucketTokenAuthUsername {
		server := c.Server
		if server == "" || server == "https://bitbucket.org" {
			server = "https://api.bitbucket.org"
		}
		client, err = bitbucket.New(server)
		if err != nil {
			return
		}
		client.Client = &http.Client{
			Transport: &transport.BearerToken{
				Token: token,
			},
		}
		return
	}

	client, err = factory.NewClient(provider, c.Server, token, func(scmClient *goscm.Client) {
		scmClient.Username = username
	})
	return
}

func (c *ClientFactory) getTokenFromSecret(secretRef *v1.SecretReference) (token, username string, err error) {
	var gitSecret *v1.Secret
	if gitSecret, err = c.getSecret(secretRef); err != nil {
		return
	}

	switch gitSecret.Type {
	case v1.SecretTypeBasicAuth, v1alpha3.SecretTypeBasicAuth:
		token = string(gitSecret.Data[v1.BasicAuthPasswordKey])
		username = string(gitSecret.Data[v1.BasicAuthUsernameKey])
	case v1.SecretTypeOpaque:
		token = string(gitSecret.Data[v1.ServiceAccountTokenKey])
	case v1alpha3.SecretTypeSecretText:
		token = string(gitSecret.Data["secret"])
	}
	return
}

// getSecret returns the secret, taking the namespace from GitRepository if it is empty
func (c *ClientFactory) getSecret(ref *v1.SecretReference) (secret *v1.Secret, err error) {
	secret = &v1.Secret{}
	ns := ref.Namespace

	if err = c.k8sClient.Get(context.TODO(), types.NamespacedName{
		Namespace: ns, Name: ref.Name,
	}, secret); err != nil {
		err = fmt.Errorf("cannot get secret %v, error is: %v", secret, err)
	}
	return
}

// ResourceGetter represent the interface for getting Kubernetes resource
type ResourceGetter interface {
	Get(ctx context.Context, key types.NamespacedName, obj client.Object) error
}

// bitbucketUserWorkspacesResponse is the API response from GET 2.0/user/workspaces,
// which supports Bitbucket Cloud HTTP Access Token (Bearer) authentication.
// The response has workspace info nested under a "workspace" key in each value.
type bitbucketUserWorkspacesResponse struct {
	Values []struct {
		Workspace struct {
			Slug string `json:"slug"`
			Name string `json:"name"`
		} `json:"workspace"`
	} `json:"values"`
}

// ListBitbucketUserWorkspaces calls GET 2.0/user/workspaces using the go-scm
// client's existing HTTP transport (Bearer Token configured). This endpoint
// supports Bitbucket Cloud HTTP Access Tokens, unlike 2.0/workspaces which
// does not support Bearer auth and is used by go-scm's Organizations.List.
func ListBitbucketUserWorkspaces(ctx context.Context, c *goscm.Client) ([]*goscm.Organization, *goscm.Response, error) {
	req := &goscm.Request{
		Method: "GET",
		Path:   "2.0/user/workspaces",
	}
	resp, err := c.Do(ctx, req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()

	scmResp := &goscm.Response{
		Status: resp.Status,
		Header: resp.Header,
	}

	if resp.Status > 299 {
		return nil, scmResp, fmt.Errorf("%s", http.StatusText(resp.Status))
	}

	var result bitbucketUserWorkspacesResponse
	if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, scmResp, err
	}

	orgs := make([]*goscm.Organization, 0, len(result.Values))
	for _, v := range result.Values {
		slug := v.Workspace.Slug
		orgs = append(orgs, &goscm.Organization{
			Name:   slug,
			Avatar: fmt.Sprintf("https://bitbucket.org/workspaces/%s/avatar", slug),
		})
	}
	return orgs, scmResp, nil
}
