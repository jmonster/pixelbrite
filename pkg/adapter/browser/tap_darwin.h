#include <stdint.h>

void *pb_tap_open(int parent, int *status);
double pb_tap_rate(void *tap);
int pb_tap_read(void *tap, float *out, int max);
uint64_t pb_tap_dropped(void *tap);
void pb_tap_close(void *tap);
