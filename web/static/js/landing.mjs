// Landing page behavior (SSR shell owned) — per docs/web-ui-spec.md §1.3:
// in-page anchor scrolls that respect prefers-reduced-motion, and a
// cosmetic "Redirecting to Amazon…" swap on the LWA links (the page
// navigates away immediately after; this is not a real loading state).

const reducedMotion = window.matchMedia("(prefers-reduced-motion: reduce)");

function scrollToId(id, block) {
  const el = document.getElementById(id);
  if (!el) return;
  el.scrollIntoView({
    behavior: reducedMotion.matches ? "auto" : "smooth",
    block: block || "start",
  });
}

for (const btn of document.querySelectorAll("[data-scroll-to]")) {
  btn.addEventListener("click", (ev) => {
    // Real anchors still work without JS; with JS we control the block
    // position and honor reduced motion.
    ev.preventDefault();
    scrollToId(btn.getAttribute("data-scroll-to"), btn.getAttribute("data-scroll-block"));
  });
}

for (const link of document.querySelectorAll("[data-lwa-cta]")) {
  link.addEventListener("click", () => {
    link.setAttribute("aria-busy", "true");
    const label = link.querySelector("[data-lwa-label]");
    if (label) label.textContent = "Redirecting to Amazon…";
  });
}
