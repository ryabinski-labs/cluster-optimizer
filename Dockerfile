FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cluster-optimizer ./cmd/cluster-optimizer

FROM gcr.io/distroless/static:nonroot AS runtime
COPY --from=build /out/cluster-optimizer /cluster-optimizer

ENTRYPOINT ["/cluster-optimizer"]
