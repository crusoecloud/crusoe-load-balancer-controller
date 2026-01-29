# Crusoe Load Balancer Controller

This repository defines the **official Crusoe Load Balancer Controller** for use with [Crusoe Cloud](https://www.crusoecloud.com), the world's first carbon-reducing, low-cost GPU cloud platform.

The controller supports both Crusoe-managed Kubernetes clusters and self-managed Kubernetes clusters.

---

## Getting Started

Please follow the [Helm installation instructions](https://github.com/crusoecloud/crusoe-load-balancer-controller-helm-charts) to install the Load Balancer Controller.

---

## Cluster Support

### Crusoe-Managed Clusters
For Crusoe-managed Kubernetes clusters, the controller automatically detects the cluster and uses the appropriate VPC/subnet information. No additional configuration is required.

### Self-Managed Clusters
For self-managed Kubernetes clusters, you need to pass the subnet id as part of your value.yml when doing the helm install of the controller. You also need to create some secrets in your k8s cluster like so:

```
kubectl create secret generic crusoe-secrets --from-literal=CRUSOE_ACCESS_KEY="your-access-key" --from-literal=CRUSOE_SECRET_KEY="your-secret-key" --from-literal=CRUSOE_PROJECT_ID="your-project-id" -n crusoe-system
```

See example values.yml here: @https://github.com/crusoecloud/crusoe-load-balancer-controller-helm-charts/blob/0e57d2104cd98f93d13a773e50e78e2968c51855/charts/crusoe-lb-controller/values.yaml#L27

---

## Firewall Rules

To add a firewall rule to allow traffic to your load balancer based on your service's nodeports, you can use the following annotations in your Service manifest:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service
  annotations:
    crusoe.ai/create-firewall-rule: "true" # Enabled only if true
    crusoe.ai/create-firewall-rule-sources: "0.0.0.0/0" # Comma-separated list of CIDR blocks
    crusoe.ai/create-firewall-rule-protocols: "TCP,UDP" # Comma-separated list of protocols
spec:
  type: LoadBalancer
  ports:
    - port: 80
      targetPort: 80
    - port: 443
      targetPort: 443
```

The controller will automatically create a firewall rule with the specified sources and protocols.  