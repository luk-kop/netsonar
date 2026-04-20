FROM python:3.12-alpine

WORKDIR /app
COPY fake_targets.py .
COPY fake-targets.crt fake-targets.key ./

EXPOSE 8080 8888 9000 9443
CMD ["python", "/app/fake_targets.py"]
