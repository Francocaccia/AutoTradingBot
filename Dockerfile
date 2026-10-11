# Base con Python (mejor compatibilidad)
FROM python:3.11-slim

# Instalar Go
RUN apt-get update && apt-get install -y golang

WORKDIR /app

# Copiar todo
COPY . .

# Instalar dependencias Python (ahora sí funciona)
RUN pip install --no-cache-dir yfinance pandas

# Build Go
RUN go build -o bot ./cmd/bot

CMD ["./bot"]