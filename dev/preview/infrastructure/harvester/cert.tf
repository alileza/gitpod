resource "kubernetes_manifest" "cert" {
  provider = k8s.dev
  manifest = {
    apiVersion = "cert-manager.io/v1"
    kind       = "Certificate"

    metadata = {
      annotations = {
        "preview/owner" = var.preview_name
      }
      name      = "harvester-${var.preview_name}"
      namespace = "certs"
    }

    spec = {
      dnsNames = [
        "${var.preview_name}.preview.gitpod-dev.com",
        "*.${var.preview_name}.preview.gitpod-dev.com",
        "*.ws.${var.preview_name}.preview.gitpod-dev.com"
      ]
      issuerRef = {
        kind = "ClusterIssuer"
        name = "letsencrypt-issuer-gitpod-core-dev"
      }
      renewBefore = "24h0m0s"
      secretName  = "harvester-${var.preview_name}"
    }
  }
}
