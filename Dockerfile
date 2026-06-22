FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/cluster-optimizer ./cmd/cluster-optimizer
# node-image-gc is the opt-in node disk / image garbage collector that runs as
# a DaemonSet (manifests/daemonset-image-gc.yaml). It ships in the same image
# and is selected via `command: ["/node-image-gc"]`.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/node-image-gc ./cmd/node-image-gc

FROM gcr.io/distroless/static:nonroot AS runtime
COPY --from=build /out/cluster-optimizer /cluster-optimizer
COPY --from=build /out/node-image-gc /node-image-gc

# Default entrypoint is the optimizer (CronJob). The node-image-gc DaemonSet
# overrides it with its own command; that pod sets runAsUser:0 because it must
# reach the root-owned containerd socket.
ENTRYPOINT ["/cluster-optimizer"]
