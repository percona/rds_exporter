FROM golang:1.11.2 AS builder

WORKDIR /go/src/github.com/hellofresh/rds_exporter
COPY . ./
RUN make

FROM ubuntu:bionic
COPY --from=builder /go/src/github.com/hellofresh/rds_exporter/out/rds_exporter /
COPY config.yml           /etc/rds_exporter/config.yml

EXPOSE 9042
ENTRYPOINT  [ "/bin/rds_exporter", "--config.file=/etc/rds_exporter/config.yml" ]
