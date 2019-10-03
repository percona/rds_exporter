FROM golang:1.12 as builder

WORKDIR /opt/rds_exporter/
COPY go.mod .
COPY go.sum .
# Make modules cache
RUN go mod download
COPY . .
RUN make build

# Copy to a fresh image
FROM golang:1.12
COPY --from=builder /opt/rds_exporter/rds_exporter /usr/bin/rds_exporter