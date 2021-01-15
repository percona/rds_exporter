FROM golang:alpine AS builder
WORKDIR /usr/src/app
COPY . .
RUN go build -o rds_exporter .

FROM alpine:latest
RUN apk --no-cache add ca-certificates && update-ca-certificates

USER nobody
COPY --from=builder /usr/src/app/rds_exporter /bin/rds_exporter

EXPOSE     9042
ENTRYPOINT ["/bin/rds_exporter", "--config.file=/etc/rds_exporter/config.yml"]
