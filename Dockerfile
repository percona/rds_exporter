FROM        alpine:latest

COPY *_exporter  /bin/
COPY config.yml           /etc/cloudwatch_exporter/config.yml

RUN apk update && \
    apk add ca-certificates && \
    update-ca-certificates

EXPOSE      9042
ENTRYPOINT  [ "/bin/cloudwatch_exporter", "-config.file=/etc/cloudwatch_exporter/config.yml" ]