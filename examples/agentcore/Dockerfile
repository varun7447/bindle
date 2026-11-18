# AgentCore Runtime runs ARM64 containers serving HTTP on 0.0.0.0:8080
# (/invocations and /ping). The AgentCore SDK binds 0.0.0.0 inside a
# container; nothing here is published outside the runtime.
FROM --platform=linux/arm64 python:3.12-slim
RUN apt-get update && apt-get install -y --no-install-recommends git && rm -rf /var/lib/apt/lists/*
WORKDIR /app
COPY requirements.txt .
RUN pip install --no-cache-dir -r requirements.txt
COPY agent.py .
RUN useradd --uid 10001 agent
USER agent
EXPOSE 8080

# Bindle only (set DISABLE_ADOT_OBSERVABILITY=true on the runtime):
CMD ["python", "agent.py"]
# Bindle and CloudWatch: use ADOT's auto-instrumentation instead.
# CMD ["opentelemetry-instrument", "python", "agent.py"]
