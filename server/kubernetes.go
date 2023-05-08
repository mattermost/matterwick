package server

import (
	"context"
	"encoding/base64"
	"log"
	"os"
	"time"

	"github.com/aws/aws-sdk-go/aws"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/eks"
	"github.com/mattermost/mattermost-cloud/k8s"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	corev1 "k8s.io/api/core/v1"
	k8sErrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/aws-iam-authenticator/pkg/token"
)

// Deployment contains information needed to create a deployment in Kubernetes
type Deployment struct {
	Namespace      string
	PR             int
	ImageTag       string
	DeployFilePath string
	Environment    CWS
}

func namespaceExists(kc *k8s.KubeClient, namespaceName string) (bool, error) {
	_, err := kc.Clientset.CoreV1().Namespaces().Get(context.Background(), namespaceName, metav1.GetOptions{})
	if err != nil && !k8sErrors.IsNotFound(err) {
		return false, err
	} else if err != nil && k8sErrors.IsNotFound(err) {
		return false, nil
	}
	return true, nil
}

func getOrCreateNamespace(kc *k8s.KubeClient, namespaceName string) (*corev1.Namespace, error) {
	namespace, err := kc.Clientset.CoreV1().Namespaces().Get(context.Background(), namespaceName, metav1.GetOptions{})
	if err != nil && k8sErrors.IsNotFound(err) {
		return kc.CreateOrUpdateNamespace(namespaceName)
	}

	if err != nil && !k8sErrors.IsNotFound(err) {
		return nil, err
	}

	return namespace, nil
}

func deleteNamespace(kc *k8s.KubeClient, namespace string) error {
	policy := metav1.DeletePropagationForeground
	gracePeriod := int64(0)
	deleteOpts := &metav1.DeleteOptions{
		GracePeriodSeconds: &gracePeriod,
		PropagationPolicy:  &policy,
	}
	err := kc.Clientset.CoreV1().Namespaces().Delete(context.Background(), namespace, *deleteOpts)
	if err != nil {
		return errors.Wrapf(err, "failed to delete the namespace %s", namespace)
	}

	return nil
}

func waitForIPAssignment(kc *k8s.KubeClient, namespace string, logger logrus.FieldLogger) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	for {
		select {
		case <-ctx.Done():
			return "", errors.New("Timed out waiting for IP Assignment")
		case <-time.After(30 * time.Second):
			lb, _ := kc.Clientset.CoreV1().Services(namespace).Get(context.Background(), "cws-test-service", metav1.GetOptions{})

			if len(lb.Status.LoadBalancer.Ingress) > 0 {
				return lb.Status.LoadBalancer.Ingress[0].Hostname, nil
			}

			logger.Debug("No IP found yet. Waiting...")
		}
	}
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
