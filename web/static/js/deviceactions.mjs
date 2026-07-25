// Defers a device-local session action across the two response boundaries in
// a tool round trip:
//   1. the response that emitted the function call finishes;
//   2. the follow-up response, created after function_call_output, speaks its
//      acknowledgement and finishes.
//
// Acting on boundary 1 cuts off the acknowledgement the user is meant to hear.
export function createDeferredDeviceActionGate(onReady) {
  let pending = '';
  let callingResponseFinished = false;

  const clear = () => {
    pending = '';
    callingResponseFinished = false;
  };

  return {
    queue(action) {
      pending = action || '';
      callingResponseFinished = false;
    },

    responseDone() {
      if (!pending) return;
      if (!callingResponseFinished) {
        callingResponseFinished = true;
        return;
      }
      const action = pending;
      clear();
      onReady(action);
    },

    clear,
  };
}
