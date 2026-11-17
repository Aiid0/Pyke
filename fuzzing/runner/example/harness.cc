#include <stdint.h>
#include <stddef.h>
#include <stdlib.h>
// A deliberately faulty fixture, not a production target.
extern "C" int LLVMFuzzerTestOneInput(const uint8_t *data, size_t size) {
  if (size >= 4 && data[0]=='P' && data[1]=='Y' && data[2]=='K' && data[3]=='E') {
#ifndef PYKE_SAFE_FIXTURE
    volatile char *p = (char*)malloc(1);
    p[size + 32] = 1;
    free((void*)p);
#else
    volatile unsigned value = data[0] + data[1];
    (void)value;
#endif
  }
  return 0;
}
