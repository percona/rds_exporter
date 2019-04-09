FROM golang:1.11.2 AS builder

WORKDIR /go/src/github.com/hellofresh/rds_exporter
COPY . ./
RUN make

FROM ubuntu:bionic

RUN apt-get update -y && apt-get install -y ca-certificates python3-boto3 python3-yaml && rm -rf /var/cache/apt/*

COPY --from=builder /go/src/github.com/hellofresh/rds_exporter/rds_exporter /
COPY entry.py             /

EXPOSE 9042
ENTRYPOINT ["/usr/bin/python3", "/entry.py"]
