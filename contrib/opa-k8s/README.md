# Example Kubernetes Configs for EntroQ/OPA

To generate example configs from the OPA data files elsewhere, run the script

```bash
$ ./make-rego-configmaps.sh
```

This creates configmap that can be applied to allow the example OPA deployment to have configuration pulled into mounted configmap volumes.


