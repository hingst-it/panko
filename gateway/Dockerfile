FROM python:3.12-alpine
WORKDIR /app
COPY gateway/requirements.txt ./
RUN pip install --no-cache-dir -r requirements.txt
COPY gateway/*.py ./
USER 1000
ENV PYTHONUNBUFFERED=1
CMD ["uvicorn", "app:app", "--host", "0.0.0.0", "--port", "8080"]
