# Build from the project src directory; no image is deployed automatically.
FROM node:22-alpine AS frontend
WORKDIR /build/frontend
COPY frontend/package*.json ./
RUN npm ci
COPY frontend/ ./
RUN npm run build

FROM golang:1.26-alpine AS backend
WORKDIR /build/backend
ARG VERSION=1.0.0
COPY backend/ ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/db-relay .

FROM alpine:3.23
RUN apk add --no-cache ca-certificates \
    && addgroup -g 10001 relay \
    && adduser -D -u 10001 -G relay relay \
    && mkdir -p /data /app/web \
    && chown 10001:10001 /data \
    && chmod 700 /data
WORKDIR /app
COPY --from=backend /out/db-relay /app/db-relay
COPY --from=frontend /build/frontend/dist/ /app/web/
ENV LISTEN_ADDR=0.0.0.0:8080 APP_DATA_DIR=/data WEB_DIR=/app/web
USER 10001:10001
EXPOSE 8080 3306-3310
ENTRYPOINT ["/app/db-relay"]
