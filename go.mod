module github.com/mattermost/matterwick

go 1.14

require (
	github.com/aws/aws-sdk-go v1.33.14
	github.com/braintree/manners v0.0.0-20160418043613-82a8879fc5fd
	github.com/golang/protobuf v1.3.3 // indirect
	github.com/google/go-github/v28 v28.1.1
	github.com/gorilla/mux v1.7.4
	github.com/heroku/docker-registry-client v0.0.0-20190909225348-afc9e1acc3d5
	github.com/jetstack/cert-manager v0.14.0 // indirect
	github.com/kr/pretty v0.2.0 // indirect
	github.com/mattermost/ldap v3.0.4+incompatible // indirect
	github.com/mattermost/mattermost-cloud v0.23.1
	github.com/mattermost/mattermost-server/v5 v5.20.0-rc4
	github.com/pkg/errors v0.9.1
	github.com/robfig/cron/v3 v3.0.0
	github.com/sirupsen/logrus v1.6.0
	github.com/stretchr/testify v1.6.1
	golang.org/x/oauth2 v0.0.0-20200107190931-bf48bf16ab8d
	golang.org/x/sys v0.0.0-20200212091648-12a6c2dcc1e4 // indirect
	k8s.io/api v0.17.7
	k8s.io/apimachinery v0.17.7
	k8s.io/client-go v12.0.0+incompatible
	k8s.io/utils v0.0.0-20200124190032-861946025e34 // indirect
	sigs.k8s.io/aws-iam-authenticator v0.5.1
	sigs.k8s.io/yaml v1.2.0 // indirect
)

replace (
	github.com/docker/docker => github.com/moby/moby v0.7.3-0.20190826074503-38ab9da00309
	github.com/googleapis/gnostic => github.com/googleapis/gnostic v0.4.0
	k8s.io/api => k8s.io/api v0.17.7
	k8s.io/apiextensions-apiserver => k8s.io/apiextensions-apiserver v0.17.7
	k8s.io/apimachinery => k8s.io/apimachinery v0.17.7
	k8s.io/apiserver => k8s.io/apiserver v0.17.7
	k8s.io/cli-runtime => k8s.io/cli-runtime v0.17.7
	k8s.io/client-go => k8s.io/client-go v0.17.7
	k8s.io/cloud-provider => k8s.io/cloud-provider v0.17.7
	k8s.io/cluster-bootstrap => k8s.io/cluster-bootstrap v0.17.7
	k8s.io/code-generator => k8s.io/code-generator v0.17.7
	k8s.io/component-base => k8s.io/component-base v0.17.7
	k8s.io/cri-api => k8s.io/cri-api v0.17.7
	k8s.io/csi-translation-lib => k8s.io/csi-translation-lib v0.17.7
	k8s.io/kube-aggregator => k8s.io/kube-aggregator v0.17.7
	k8s.io/kube-controller-manager => k8s.io/kube-controller-manager v0.17.7
	k8s.io/kube-proxy => k8s.io/kube-proxy v0.17.7
	k8s.io/kube-scheduler => k8s.io/kube-scheduler v0.17.7
	k8s.io/kubectl => k8s.io/kubectl v0.17.7
	k8s.io/kubelet => k8s.io/kubelet v0.17.7
	k8s.io/legacy-cloud-providers => k8s.io/legacy-cloud-providers v0.17.7
	k8s.io/metrics => k8s.io/metrics v0.17.7
	k8s.io/node-api => k8s.io/node-api v0.17.7
	k8s.io/sample-apiserver => k8s.io/sample-apiserver v0.17.7
	k8s.io/sample-cli-plugin => k8s.io/sample-cli-plugin v0.17.7
	k8s.io/sample-controller => k8s.io/sample-controller v0.17.7
)
