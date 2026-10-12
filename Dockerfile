# Python base image (yfinance/pandas wheels install cleanly), plus Go for the build
FROM python:3.11-slim

RUN apt-get update && apt-get install -y golang

WORKDIR /app
COPY . .

RUN pip install --no-cache-dir yfinance pandas

RUN go build -o bot ./cmd/bot

CMD ["./bot"]
