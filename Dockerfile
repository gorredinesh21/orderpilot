# OrderPilot — multi-stage build, distroless runtime (~20 MB image).
FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# static binary: embed.FS ships the frontend inside the binary
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /orderpilot .

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /orderpilot /orderpilot
ENV PORT=8080
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/orderpilot"]
