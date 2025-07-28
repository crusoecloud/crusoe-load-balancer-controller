package utils

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"time"

	"github.com/briandowns/spinner"
	swagger "github.com/crusoecloud/client-go/swagger/v1alpha5"
	"github.com/ory/viper"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type opStatus string

const (
	maxBackOffSeconds                   = 8
	spinnerWaitTimeMilliSecond          = 400
	jitterRangeMilliSecond              = 1000
	OpInProgress               opStatus = "IN_PROGRESS"
	CrusoeAPIEndpointFlag               = "crusoe-api-endpoint"
	CrusoeAccessKeyFlag                 = "crusoe-elb-access-key"
	CrusoeSecretKeyFlag                 = "crusoe-elb-secret-key" //nolint:gosec // false positive, this is a flag name
	CrusoeClusterIDFlag                 = "crusoe-cluster-id"     //nolint:gosec // false positive, this is a flag name
	CrusoeProjectIDFlag                 = "crusoe-project-id"
	CrusoeVPCIDFlag                     = "crusoe-vpc-id"
	NodeNameFlag                        = "node-name"
)

var (
	errOperationGet = errors.New("failed to get operation")
)

func EqualPorts(ports1, ports2 []corev1.ServicePort) bool {
	if len(ports1) != len(ports2) {
		return false
	}
	for i := range ports1 {
		if ports1[i] != ports2[i] {
			return false
		}
	}
	return true
}

func CopyPortsFromService(svc *corev1.Service) []corev1.ServicePort {
	//nolint:prealloc
	var ports []corev1.ServicePort
	for _, port := range svc.Spec.Ports {
		ports = append(ports, corev1.ServicePort{
			Name:       port.Name,
			Protocol:   port.Protocol,
			Port:       port.Port,
			TargetPort: port.TargetPort,
			NodePort:   port.NodePort,
		})
	}
	return ports
}

func EqualSelectors(selector1, selector2 map[string]string) bool {
	if len(selector1) != len(selector2) {
		return false
	}
	for key, val := range selector1 {
		if selector2[key] != val {
			return false
		}
	}
	return true
}

func jitterMillisecond(max int) time.Duration {
	bigRand, err := rand.Int(rand.Reader, big.NewInt(int64(max)))
	if err != nil {
		return time.Duration(0)
	}

	return time.Duration(bigRand.Int64()) * time.Millisecond
}

func WaitForOperation(ctx context.Context, opPrefix string, op *swagger.Operation, projectID string,
	getFunc func(context.Context, string, string) (swagger.Operation, *http.Response, error)) (
	*swagger.Operation, error,
) {
	logger := log.FromContext(ctx)
	backoffRate := 1.75
	backoffSecondsFloat := float64(1)
	maxBackoffSecondsFloat := float64(maxBackOffSeconds)
	charset := []string{"☀️  ", "☀️ 🌱", "☀️ 🌿", "☀️ 🪴", "☀️ 🌳"}
	s := spinner.New(charset, spinnerWaitTimeMilliSecond*time.Millisecond) // Build our new spinner
	s.Prefix = opPrefix + "\t"
	s.Start() // Start the spinner
	defer s.Stop()

	logger.Info("START POLL", "op_resp", op)

	for op.State == string(OpInProgress) {
		updatedOp, httpResp, err := getFunc(ctx, projectID, op.OperationId)
		if err != nil {
			return nil, fmt.Errorf("error getting operation with id %s: %w", op.OperationId, err)
		}
		closeErr := httpResp.Body.Close()
		if closeErr != nil {
			return nil, fmt.Errorf("failed to close http response body: %w", closeErr)
		}

		if httpResp.StatusCode != http.StatusOK {
			return nil, fmt.Errorf("error getting operation with id %s with HTTP status %s: %w",
				op.OperationId, httpResp.Status, errOperationGet)
		}
		op = &updatedOp
		time.Sleep(time.Duration(backoffSecondsFloat)*time.Second + jitterMillisecond(jitterRangeMilliSecond))
		backoffSecondsFloat *= backoffRate
		if backoffSecondsFloat > maxBackoffSecondsFloat {
			backoffSecondsFloat = maxBackoffSecondsFloat
		}
		logger.Info("POLLING", "op_resp", op)
	}

	return op, nil
}

func BindEnvs() error {
	if err := viper.BindEnv(CrusoeAPIEndpointFlag, "CRUSOE_API_ENDPOINT"); err != nil {
		return fmt.Errorf("bind CrusoeAPIEndpointFlag: %w", err)
	}
	if err := viper.BindEnv(CrusoeAccessKeyFlag, "CRUSOE_ACCESS_KEY"); err != nil {
		return fmt.Errorf("bind CrusoeAccessKeyFlag: %w", err)
	}
	if err := viper.BindEnv(CrusoeSecretKeyFlag, "CRUSOE_SECRET_KEY"); err != nil {
		return fmt.Errorf("Failed to bind env CRUSOE_SECRET_KEY: %v", err)
	}
	if err := viper.BindEnv(CrusoeProjectIDFlag, "CRUSOE_PROJECT_ID"); err != nil {
		return fmt.Errorf("Failed to bind env CRUSOE_PROJECT_ID: %v", err)
	}
	if err := viper.BindEnv(CrusoeVPCIDFlag, "CRUSOE_VPC_ID"); err != nil {
		return fmt.Errorf("Failed to bind env CRUSOE_VPC_ID: %v", err)
	}
	if err := viper.BindEnv(NodeNameFlag, "NODE_NAME"); err != nil {
		return fmt.Errorf("Failed to bind env NODE_NAME: %v", err)
	}
	if err := viper.BindEnv(CrusoeClusterIDFlag, "CRUSOE_CLUSTER_ID"); err != nil {
		return fmt.Errorf("Failed to bind env CRUSOE_CLUSTER_ID: %v", err)
	}
	return nil
}
