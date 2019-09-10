
FROM golang:1.12.6-alpine as build
WORKDIR /opt/
RUN apk update && apk add make git && git clone https://github.com/quintoandar/rds_exporter.git
WORKDIR /opt/rds_exporter
RUN make build
RUN chmod +x rds_exporter


FROM alpine:latest

COPY --from=build ["/opt/rds_exporter/rds_exporter", "/bin/" ]
COPY config.yml           /etc/rds_exporter/config.yml

RUN apk update && \
    apk add ca-certificates && \
    update-ca-certificates

EXPOSE      9042
ENTRYPOINT  [ "/bin/rds_exporter", "--config.file=/etc/rds_exporter/config.yml" ]
