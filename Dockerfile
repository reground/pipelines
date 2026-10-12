FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /pipelines .

FROM alpine:3.20
RUN apk add --no-cache ca-certificates
COPY --from=build /pipelines /usr/local/bin/pipelines
ENV LISTEN=0.0.0.0:8080
ENTRYPOINT ["pipelines"]
CMD ["serve"]
