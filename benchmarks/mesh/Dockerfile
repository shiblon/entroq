FROM golang:1.25-bookworm AS builder

ENV CGO_ENABLED=0

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download -x
COPY . .
RUN go build -o /bin/workload ./benchmarks/mesh/workload

FROM gcr.io/distroless/static-debian12:nonroot

LABEL org.opencontainers.image.source="https://github.com/shiblon/entroq"

COPY --from=builder /bin/workload /bin/workload

EXPOSE 8000

ENTRYPOINT ["/bin/workload"]
