# Metrics

`alitellm-operator` exposes Prometheus metrics on port `8443` (TLS) by default.

!!! warning "Placeholder"
    The full metrics catalogue is pending re-documentation against the ach
    controller set (`internal/metrics`). The source of truth lives at
    [`internal/metrics`](https://github.com/ackstorm/alitellm-operator/tree/main/internal/metrics)
    until this page is rewritten.

The standard
[controller-runtime](https://pkg.go.dev/sigs.k8s.io/controller-runtime/pkg/metrics)
metrics are also published (work-queue depth, reconcile duration, errors).
