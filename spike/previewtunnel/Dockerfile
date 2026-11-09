# Throwaway image for the real-edge run of the preview-tunnel spike (gateway mode only).
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /edgespike ./cmd/edgespike

FROM gcr.io/distroless/static:nonroot
COPY --from=build /edgespike /edgespike
USER nonroot
ENTRYPOINT ["/edgespike", "gateway", "-listen", ":8080"]
