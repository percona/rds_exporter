# Build Application
FROM		golang:alpine as builder
RUN		apk update && apk add git make
COPY		. /go/src/github.com/percona/rds_exporter/
WORKDIR		/go/src/github.com/percona/rds_exporter/
RUN		mkdir -p /go/src/github.com/percona/rds_exporter/vendor/github.com/percona
RUN		ln -s /go/src/github.com/percona/rds_exporter/vendor/github.com/percona/rds_exporter /go/src/github.com/percona/rds_exporter/
RUN		make build

# Build Docker Container
FROM		alpine:latest
COPY		--from=builder /go/src/github.com/percona/rds_exporter/rds_exporter /bin/
EXPOSE		9042
ENTRYPOINT	["/bin/rds_exporter", "--config.file=/etc/rds_exporter/config.yml"]
