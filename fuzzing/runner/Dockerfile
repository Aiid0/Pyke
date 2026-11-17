FROM rust:1.90.0-bookworm AS builder
RUN apt-get update && apt-get install -y --no-install-recommends clang llvm-dev libclang-dev cmake git python3 && rm -rf /var/lib/apt/lists/*
RUN rustup toolchain install nightly-2025-10-01 --profile minimal --component llvm-tools
RUN git clone https://github.com/AFLplusplus/LibAFL.git /opt/LibAFL && cd /opt/LibAFL && git checkout bd49bdf4f64a50349d803646fb2afa9b8b104fa5
WORKDIR /opt/LibAFL/crates/libafl_libfuzzer_runtime
RUN apt-get update && apt-get install -y --no-install-recommends libclang-rt-14-dev && rm -rf /var/lib/apt/lists/*
RUN bash build.sh --toolchain nightly-2025-10-01 --cargo-args '--features fork'

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends clang llvm-dev build-essential cmake pkg-config python3 ca-certificates && rm -rf /var/lib/apt/lists/*
RUN apt-get update && apt-get install -y --no-install-recommends libclang-rt-14-dev && rm -rf /var/lib/apt/lists/*
COPY --from=builder /opt/LibAFL/crates/libafl_libfuzzer_runtime/libFuzzer.a /opt/libafl/libFuzzer.a
COPY --from=builder /opt/LibAFL/crates/libafl_libfuzzer_runtime/Cargo.lock /opt/libafl/Cargo.lock
COPY supervisor.py /opt/libafl/supervisor.py
ENV CC=clang CXX=clang++ LIBAFL_LIB=/opt/libafl/libFuzzer.a
WORKDIR /job
ENTRYPOINT ["python3", "/opt/libafl/supervisor.py"]
