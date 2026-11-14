FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/tramite ./cmd/tramite \
 && mkdir -p /out/data

# distroless/static: CA certificates, no shell; runs as uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/tramite /tramite
COPY --from=build --chown=65532:65532 /out/data /data
COPY config.example.yaml /config/config.yaml
ENV TRAMITE_CONFIG=/config/config.yaml
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/tramite"]
