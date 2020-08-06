package server

import (
	"encoding/base64"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mattermost/mattermost-cloud/k8s"
	"github.com/mattermost/mattermost-server/v5/mlog"
	"github.com/pkg/errors"
	logrus "github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
)

// Deployment contains information needed to create a deployment in Kubernetes
type Deployment struct {
	Namespace      string
	ImageTag       string
	DeployFilePath string
	Environment    CWS
}

func namespaceExists(kc *k8s.KubeClient, namespaceName string) (bool, error) {
	_, err := kc.Clientset.CoreV1().Namespaces().Get(namespaceName, metav1.GetOptions{})
	if err != nil && !k8sErrors.IsNotFound(err) {
		return false, err
	} else if err != nil && k8sErrors.IsNotFound(err) {
		return false, nil
	}
	return true, nil
}

func getOrCreateNamespace(kc *k8s.KubeClient, namespaceName string) (*corev1.Namespace, error) {
	namespace, err := kc.Clientset.CoreV1().Namespaces().Get(namespaceName, metav1.GetOptions{})
	if err != nil && k8sErrors.IsNotFound(err) {
		return kc.CreateOrUpdateNamespace(namespaceName)
	}
	
	if err != nil && !k8sErrors.IsNotFound(err) {
		return nil, err
	}

	return namespace, nil
}

func deleteNamespace(kc *k8s.KubeClient, nameSpaceName string) error {
	namespace := []string{nameSpaceName}
	err := kc.DeleteNamespaces(namespace)
	if err != nil {
		return err
	}
	return nil
}

func waitForIPAssignment(kc *k8s.KubeClient, deployment Deployment) (string, error) {
	mlog.Info("Waiting for external IP to be assigned")
	IP := ""
	for {
		if IP != "" {
			break
		} else {
			log.Printf("No IP for now.\n")
		}

		lb, _ := kc.Clientset.CoreV1().Services(deployment.Namespace).Get("cws-test-service", metav1.GetOptions{})

		if len(lb.Status.LoadBalancer.Ingress) > 0 {
			IP = lb.Status.LoadBalancer.Ingress[0].Hostname
		}

		time.Sleep(1 * time.Second)
	}

	return IP, nil

}

func newKubeClient(cluster *eks.Cluster, logger logrus.FieldLogger) (*k8s.KubeClient, error) {
	gen, err := token.NewGenerator(true, false)
	if err != nil {
		return nil, err
	}
	opts := &token.GetTokenOptions{
		ClusterID: aws.StringValue(cluster.Name),
	}
	tok, err := gen.GetWithOptions(opts)
	if err != nil {
		return nil, err
	}
	ca, err := base64.StdEncoding.DecodeString(aws.StringValue(cluster.CertificateAuthority.Data))
	if err != nil {
		return nil, err
	}
	kc, err := k8s.NewFromConfig(
		&rest.Config{
			Host:        aws.StringValue(cluster.Endpoint),
			BearerToken: tok.Token,
			TLSClientConfig: rest.TLSClientConfig{
				CAData: ca,
			},
		},
		logger,
	)
	if err != nil {
		return nil, err
	}
	return kc, nil
}

func (s *Server) newClient(logger logrus.FieldLogger) (*k8s.KubeClient, error) {
	if !isAwsConfigDefined() {
		return nil, errors.Errorf("AWS Config not defined. Unable to authenticate with EKS")
	}

	name := s.Config.KubeClusterName
	region := s.Config.KubeClusterRegion
	sess := session.Must(session.NewSession(&aws.Config{
		Region: aws.String(region),
	}))
	eksSvc := eks.New(sess)

	input := &eks.DescribeClusterInput{
		Name: aws.String(name),
	}
	result, err := eksSvc.DescribeCluster(input)
	if err != nil {
		log.Fatalf("Error calling DescribeCluster: %v", err)
	}
	kc, err := newKubeClient(result.Cluster, logger)
	return kc, nil
}

func isAwsConfigDefined() bool {
	_, awsSecretKey := os.LookupEnv("AWS_SECRET_ACCESS_KEY")
	_, awsAccessKeyID := os.LookupEnv("AWS_ACCESS_KEY_ID")

	return awsSecretKey && awsAccessKeyID
}
