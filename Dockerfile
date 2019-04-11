FROM golang:1.11.2 AS builder

WORKDIR /go/src/github.com/hellofresh/rds_exporter
COPY . ./
RUN make

FROM alpine:3.9

RUN apk add --no-cache --repository http://dl-cdn.alpinelinux.org/alpine/edge/testing \
    ca-certificates==20190108-r0 \
    py-boto3==1.9.75-r2 \
    py-yaml==4.1-r0

COPY --from=builder /go/src/github.com/hellofresh/rds_exporter/rds_exporter /
COPY entry.py             /

EXPOSE 9042
ENTRYPOINT ["/usr/bin/python3", "/entry.py"]
