# --- build stage ---
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Pure-Go build (no CGO) → a single static binary that cross-compiles cleanly
# for arm64/armv7/amd64.
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/tideline ./cmd/tideline
RUN mkdir -p /data

# --- runtime stage ---
# distroless/static gives us CA certificates (needed for HTTPS metadata fetches)
# and a non-root user, with almost no attack surface.
FROM gcr.io/distroless/static:nonroot
COPY --from=build /out/tideline /tideline
COPY --from=build --chown=nonroot:nonroot /data /data
ENV TIDELINE_DB=/data/tideline.db \
    TIDELINE_ADDR=:8080
EXPOSE 8080
USER nonroot
VOLUME /data
ENTRYPOINT ["/tideline"]
