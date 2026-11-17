#!/usr/bin/env bash
set -eu
${CXX} ${CXXFLAGS} harness.cc "${LIBAFL_LIB}" -lpthread -ldl -lm -o "${OUT}/fuzz_target"
