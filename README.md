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

## Protocols

The controller supports both TCP and UDP load balancers. The protocol is taken from the
Service's ports — no annotation is needed:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: coredns
spec:
  type: LoadBalancer
  ports:
    - port: 53
      targetPort: 53
      protocol: UDP
```

Two constraints come from the Crusoe external load balancer API:

1. **One protocol per load balancer.** The protocol applies to the whole load balancer,
   not to individual listeners, so a Service cannot mix TCP and UDP ports. The controller
   refuses to create a load balancer for such a Service and logs why. Split it into one
   Service per protocol instead — for DNS, that means a UDP Service and a TCP Service in
   front of the same pods:

   ```
   coredns       LoadBalancer  10.233.13.73  160.211.64.65   53:30530/UDP
   coredns-tcp   LoadBalancer  10.233.7.93   216.86.175.103  53:30053/TCP
   ```

   Each gets its own VIP.

2. **The protocol is fixed at creation.** The update API carries no protocol field, so
   editing a Service's port protocol after its load balancer exists will not change the
   load balancer. The controller logs a warning; recreate the Service to change protocol.

SCTP ports are rejected — Crusoe external load balancers do not offer SCTP.

---

## Firewall Rules

To add a firewall rule to allow traffic to your load balancer based on your service's nodeports, you can use the following annotations in your Service manifest:

```yaml
apiVersion: v1
kind: Service
metadata:
  name: my-service
  annotations:
    crusoe.ai/manage-firewall-rule: "true" # Enabled only if true
spec:
  type: LoadBalancer
  ports:
    - port: 80
      targetPort: 80
  loadBalancerSourceRanges:
    - 0.0.0.0/0 # Optional: defaults to 0.0.0.0/0 if not specified
```

The controller will automatically create a firewall rule with the specified sources and protocols.  