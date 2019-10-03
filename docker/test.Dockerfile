FROM golang:1.12

WORKDIR /opt/rds_exporter/
COPY go.mod .
COPY go.sum .
# Make modules cache
RUN go mod download
COPY . .