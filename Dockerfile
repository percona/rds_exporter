FROM golang:1.17-alpine as build

COPY . /usr/src/rds_exporter
WORKDIR /usr/src/rds_exporter
RUN cd /usr/src/rds_exporter && go build

FROM alpine:latest
COPY --from=build /usr/src/rds_exporter/rds_exporter  /bin/
#COPY --from=build /usr/src/rds_exporter/config.yml /bin/

RUN apk update && \
    apk add ca-certificates && \
    update-ca-certificates

EXPOSE      9042
ENTRYPOINT  [ "/bin/rds_exporter", "--config.file=/etc/rds_exporter/config.yml", "--use-irsa" ]
