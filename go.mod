module github.com/mattermost/matterwick

go 1.14

require (
	github.com/aws/aws-sdk-go v1.34.26
	github.com/braintree/manners v0.0.0-20160418043613-82a8879fc5fd
	github.com/docker/spdystream v0.0.0-20181023171402-6480d4af844c // indirect
	github.com/google/go-github/v32 v32.1.0
	github.com/gorilla/mux v1.7.4
	github.com/heroku/docker-registry-client v0.0.0-20190909225348-afc9e1acc3d5
	github.com/kr/pretty v0.2.0 // indirect
	github.com/mattermost/customer-web-server v0.0.0-20201104103624-50966b6fa076
	github.com/mattermost/mattermost-cloud v0.29.0
	github.com/mattermost/mattermost-server/v5 v5.26.0
	github.com/pkg/errors v0.9.1
	github.com/sirupsen/logrus v1.7.0
	github.com/stretchr/testify v1.6.1
	golang.org/x/oauth2 v0.0.0-20200107190931-bf48bf16ab8d
	k8s.io/api v0.18.9
	k8s.io/apimachinery v0.18.9
	k8s.io/client-go v12.0.0+incompatible
	sigs.k8s.io/aws-iam-authenticator v0.5.1
)

replace (
	github.com/docker/docker => github.com/moby/moby v0.7.3-0.20190826074503-38ab9da00309
	github.com/googleapis/gnostic => github.com/googleapis/gnostic v0.4.0
	k8s.io/api => k8s.io/api v0.18.2
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.18.2
	k8s.io/apimachinery => k8s.io/apimachinery v0.18.2
	k8s.io/apiserver => k8s.io/apiserver v0.18.2
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.18.2
	k8s.io/client-go => k8s.io/client-go v0.18.2
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.18.2
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.18.2
	k8s.io/code-generator => k8s.io/code-generator v0.18.2
	k8s.io/component-base => k8s.io/component-base v0.18.2
	k8s.io/cri-api => k8s.io/cri-api v0.18.2
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.18.2
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.18.2
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.18.2
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.18.2
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.18.2
	k8s.io/kubectl => k8s.io/kubectl v0.18.2
	k8s.io/kubelet => k8s.io/kubelet v0.18.2
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.18.2
	k8s.io/metrics => k8s.io/metrics v0.18.2
	k8s.io/node-api => k8s.io/node-api v0.18.2
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.18.2
	k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.18.2
	k8s.io/sample-controller => k8s.io/sample-controller v0.18.2
)
