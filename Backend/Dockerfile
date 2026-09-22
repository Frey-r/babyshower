# ---------- build del front (Astro) ----------
FROM node:20-alpine AS front
WORKDIR /front
COPY Front/package.json Front/package-lock.json* ./
RUN npm ci --no-audit --no-fund || npm install --no-audit --no-fund
COPY Front/astro.config.mjs Front/tsconfig.json ./
COPY Front/public ./public
COPY Front/src ./src
RUN npm run build

# ---------- build del backend (Go) ----------
FROM golang:1.24-alpine AS build
WORKDIR /src
COPY Backend/go.mod Backend/go.sum* ./
RUN go mod download
COPY Backend/ ./
RUN rm -rf internal/web/dist
COPY --from=front /front/dist ./internal/web/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /app ./cmd/server

# ---------- runtime ----------
FROM alpine:3.20
RUN adduser -D -u 10001 app
USER app
COPY --from=build /app /app
EXPOSE 8080
ENTRYPOINT ["/app"]
