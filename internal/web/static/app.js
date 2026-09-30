// Show that magnetowid is unreachable until a refresh gets a response again.
document.addEventListener("htmx:error", (e) => {
  if (!e.detail.ctx?.response) document.documentElement.dataset.offline = "";
});
document.addEventListener("htmx:after:request", () => {
  delete document.documentElement.dataset.offline;
});

// Refreshes come every 2 s, so between them advance the running download at
// its average rate, and ease towards each new reading instead of jumping.
// Progress never moves backwards within a job; a new job gets a new bar.
{
  const maxLead = 4; // seconds to extrapolate past the last reading
  let bar, reading, rate, readAt, shown, last;

  const frame = (now) => {
    const el = document.querySelector(".progress[data-fraction]");
    if (el) {
      const f = parseFloat(el.dataset.fraction) || 0;
      if (el !== bar) {
        bar = el;
        shown = f;
        reading = null;
      }
      if (f !== reading) {
        reading = f;
        rate = parseFloat(el.dataset.rate) || 0;
        readAt = now;
        // A big drop means the reading is right and the display is not.
        if (f < shown - 0.05) shown = f;
      }
      const ahead = rate * Math.min((now - readAt) / 1000, maxLead);
      const target = Math.min(reading + ahead, 0.999);
      const dt = last ? now - last : 16;
      if (target > shown) shown += (target - shown) * Math.min(1, dt / 250);
      const pct = Math.floor(shown * 100);
      el.value = shown * 100;
      const num = el.parentElement.querySelector(".percent-num");
      if (num && num.textContent !== String(pct)) num.textContent = pct;
      if (/^\d+%/.test(document.title) && !document.title.startsWith(pct + "%")) {
        document.title = document.title.replace(/^\d+%/, pct + "%");
      }
    } else {
      bar = null;
    }
    last = now;
    requestAnimationFrame(frame);
  };
  requestAnimationFrame(frame);
}
