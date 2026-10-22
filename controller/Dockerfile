FROM python:3.12-alpine
WORKDIR /app
COPY controller/hook.py /app/hook.py
USER 1000
ENV PYTHONUNBUFFERED=1
CMD ["python3", "/app/hook.py"]
