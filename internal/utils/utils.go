package utils

import (
	corev1 "k8s.io/api/core/v1"
)

const (
	maxNameLength = 63
	suffix        = "-nodeport"
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

// https://stackoverflow.com/a/59917275, Service names are restricted to a maximum of 63 characters
func GenerateNodePortServiceName(baseName string) string {
	if len(baseName)+len(suffix) > maxNameLength {
		baseName = baseName[:maxNameLength-len(suffix)]
	}
	return baseName + suffix
}
