FROM registry.access.redhat.com/ubi9/ubi-minimal AS certs

FROM registry.access.redhat.com/ubi9/ubi-micro
ARG TARGETPLATFORM
# GoReleaser stages the binaries under $TARGETPLATFORM in its build context.
ARG BIN_DIR=$TARGETPLATFORM

LABEL name="Percona RDS Exporter" \
      vendor="Percona" \
      summary="Prometheus exporter for Amazon RDS and Aurora" \
      description="Prometheus exporter for Amazon RDS and Aurora (CloudWatch and Enhanced Monitoring)" \
      maintainer="Percona Development <info@percona.com>"

# ubi-micro ships no CA bundle; take the one from ubi-minimal.
COPY --from=certs /etc/pki/ca-trust/extracted/pem/tls-ca-bundle.pem /etc/pki/tls/certs/ca-bundle.crt
COPY LICENSE /licenses/
COPY $BIN_DIR/rds_exporter /
USER 65535:65535
EXPOSE 9042
ENTRYPOINT ["/rds_exporter", "--config.file=/etc/rds_exporter/config.yml"]
