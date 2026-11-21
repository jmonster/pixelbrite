// A private, muted Core Audio tap of one process (macOS 14.2+). The IOProc
// only copies floats into a single-producer ring; Go drains it.
#import <CoreAudio/CoreAudio.h>
#import <CoreAudio/CATapDescription.h>
#import <CoreAudio/AudioHardwareTapping.h>
#import <Foundation/Foundation.h>
#include <stdatomic.h>
#include <libproc.h>
#include "tap_darwin.h"

#define RING_SECONDS 4

typedef struct {
    AudioObjectID tap, device;
    AudioDeviceIOProcID io;
    double rate;
    float *ring;   // RING_SECONDS of stereo at the tap's rate
    uint64_t size; // floats in ring
    _Atomic uint64_t read, write, dropped;
    void *description; // retained CATapDescription
} pb_tap;

static OSStatus capture(AudioObjectID dev, const AudioTimeStamp *now, const AudioBufferList *in,
                        const AudioTimeStamp *inTime, AudioBufferList *out, const AudioTimeStamp *outTime, void *ctx) {
    pb_tap *t = ctx;
    if (!in || in->mNumberBuffers != 1) return noErr;
    const float *data = in->mBuffers[0].mData;
    size_t count = in->mBuffers[0].mDataByteSize / sizeof(float);
    uint64_t w = atomic_load_explicit(&t->write, memory_order_relaxed);
    uint64_t r = atomic_load_explicit(&t->read, memory_order_acquire);
    if (!data) return noErr;
    if (count > t->size - (w - r)) { // consumer fell behind: drop this buffer and count it
        atomic_fetch_add_explicit(&t->dropped, count, memory_order_relaxed);
        return noErr;
    }
    for (size_t i = 0; i < count; i++) t->ring[(w + i) % t->size] = data[i];
    atomic_store_explicit(&t->write, w + count, memory_order_release);
    return noErr;
}

static AudioObjectPropertyAddress global(AudioObjectPropertySelector s) {
    return (AudioObjectPropertyAddress){s, kAudioObjectPropertyScopeGlobal, kAudioObjectPropertyElementMain};
}

// children returns the audio process objects of parent's children once one
// of them is playing, or nil: Chrome plays from a helper process it starts.
static NSArray<NSNumber *> *children(int parent) API_AVAILABLE(macos(14.2)) {
    AudioObjectPropertyAddress a = global(kAudioHardwarePropertyProcessObjectList);
    UInt32 size = 0;
    if (AudioObjectGetPropertyDataSize(kAudioObjectSystemObject, &a, 0, NULL, &size) || size == 0) return nil;
    AudioObjectID *objects = malloc(size);
    if (AudioObjectGetPropertyData(kAudioObjectSystemObject, &a, 0, NULL, &size, objects)) { free(objects); return nil; }
    NSMutableArray<NSNumber *> *found = [NSMutableArray array];
    BOOL playing = NO;
    for (UInt32 i = 0; i < size / sizeof *objects; i++) {
        pid_t pid = 0;
        UInt32 n = sizeof pid;
        AudioObjectPropertyAddress p = global(kAudioProcessPropertyPID);
        if (AudioObjectGetPropertyData(objects[i], &p, 0, NULL, &n, &pid)) continue;
        struct proc_bsdinfo info;
        if (proc_pidinfo(pid, PROC_PIDTBSDINFO, 0, &info, sizeof info) != sizeof info || (int)info.pbi_ppid != parent) continue;
        [found addObject:@(objects[i])];
        UInt32 running = 0;
        n = sizeof running;
        p = global(kAudioProcessPropertyIsRunningOutput);
        if (!AudioObjectGetPropertyData(objects[i], &p, 0, NULL, &n, &running) && running) playing = YES;
    }
    free(objects);
    return playing ? found : nil;
}

void *pb_tap_open(int parent, int *status) {
    *status = 0;
    if (@available(macOS 14.2, *)) {
        NSArray<NSNumber *> *processes = children(parent);
        if (!processes) return NULL; // nothing plays yet
        UInt32 size;
        AudioObjectPropertyAddress a;
        pb_tap *t = calloc(1, sizeof *t);
        CATapDescription *d = [[CATapDescription alloc] initStereoMixdownOfProcesses:processes];
        d.name = @"Pixel Brite browser";
        d.privateTap = YES;
        d.muteBehavior = CATapMutedWhenTapped;
        t->description = (__bridge_retained void *)d;
        OSStatus s = AudioHardwareCreateProcessTap(d, &t->tap);
        AudioStreamBasicDescription f = {0};
        size = sizeof f;
        a = global(kAudioTapPropertyFormat);
        if (!s) s = AudioObjectGetPropertyData(t->tap, &a, 0, NULL, &size, &f);
        if (!s && (f.mChannelsPerFrame != 2 || !(f.mFormatFlags & kAudioFormatFlagIsFloat))) s = kAudioHardwareUnsupportedOperationError;
        t->rate = f.mSampleRate;
        if (!s) {
            t->size = (uint64_t)(f.mSampleRate * 2 * RING_SECONDS);
            t->ring = calloc(t->size, sizeof(float));
            if (!t->ring) s = kAudioHardwareUnspecifiedError;
        }
        NSDictionary *agg = @{@kAudioAggregateDeviceNameKey: @"Pixel Brite browser (private)",
                              @kAudioAggregateDeviceUIDKey: NSUUID.UUID.UUIDString,
                              @kAudioAggregateDeviceIsPrivateKey: @YES,
                              @kAudioAggregateDeviceTapAutoStartKey: @YES,
                              @kAudioAggregateDeviceTapListKey: @[@{@kAudioSubTapUIDKey: d.UUID.UUIDString,
                                                                   @kAudioSubTapDriftCompensationKey: @YES}]};
        if (!s) s = AudioHardwareCreateAggregateDevice((__bridge CFDictionaryRef)agg, &t->device);
        if (!s) s = AudioDeviceCreateIOProcID(t->device, capture, t, &t->io);
        if (!s) s = AudioDeviceStart(t->device, t->io);
        if (s) { *status = s; pb_tap_close(t); return NULL; }
        return t;
    }
    *status = kAudioHardwareUnsupportedOperationError;
    return NULL;
}

double pb_tap_rate(void *h) { return ((pb_tap *)h)->rate; }

uint64_t pb_tap_dropped(void *h) { return atomic_load_explicit(&((pb_tap *)h)->dropped, memory_order_relaxed); }

int pb_tap_read(void *h, float *out, int max) {
    pb_tap *t = h;
    uint64_t r = atomic_load_explicit(&t->read, memory_order_relaxed);
    uint64_t w = atomic_load_explicit(&t->write, memory_order_acquire);
    int n = (int)((w - r) < (uint64_t)max ? (w - r) : (uint64_t)max);
    for (int i = 0; i < n; i++) out[i] = t->ring[(r + i) % t->size];
    atomic_store_explicit(&t->read, r + n, memory_order_release);
    return n;
}

void pb_tap_close(void *h) {
    pb_tap *t = h;
    if (@available(macOS 14.2, *)) {
        if (t->io) { AudioDeviceStop(t->device, t->io); AudioDeviceDestroyIOProcID(t->device, t->io); }
        if (t->device) AudioHardwareDestroyAggregateDevice(t->device);
        if (t->tap) AudioHardwareDestroyProcessTap(t->tap);
    }
    if (t->description) CFRelease(t->description);
    free(t->ring);
    free(t);
}
