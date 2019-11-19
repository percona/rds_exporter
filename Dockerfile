FROM golang:1.13-alpine AS builder
WORKDIR /usr/src/app
COPY . .
RUN go build -o rds_exporter .

FROM alpine:3.10
RUN apk --no-cache add ca-certificates && update-ca-certificates

USER nobody
COPY --from=builder /usr/src/app/rds_exporter /bin/rds_exporter
COPY config.yml /etc/rds_exporter/config.yml

EXPOSE     9042
ENTRYPOINT ["/bin/rds_exporter", "--config.file=/etc/rds_exporter/config.yml"]
