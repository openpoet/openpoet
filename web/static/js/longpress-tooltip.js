/**
 * Long-Press Tooltip Framework
 *
 * App-scoped, document-level event delegation for touch long-press tooltips.
 * Automatically applies to any button with a `title` or `data-tooltip` attribute.
 * No per-button wiring needed — just add a title attribute to your button.
 *
 * Reuses the `.icon-tooltip` CSS class defined in app.css.
 */
(function () {
    const LONG_PRESS_MS = 400;
    const AUTO_DISMISS_MS = 1200;
    const SELECTOR = 'button[title], button[data-tooltip], [role="button"][title], [role="button"][data-tooltip]';

    let pressTimer = null;
    let dismissTimer = null;
    let tooltip = null;
    let activeBtn = null;

    function showTooltip(btn) {
        const text = btn.dataset.tooltip || btn.title;
        if (!text) return;
        removeTooltip();

        tooltip = document.createElement('div');
        tooltip.className = 'icon-tooltip';
        tooltip.textContent = text;
        document.body.appendChild(tooltip);

        const rect = btn.getBoundingClientRect();
        const tooltipRect = tooltip.getBoundingClientRect();

        // Position above button, centered horizontally
        let left = rect.left + rect.width / 2;
        let top = rect.top - 8;

        // If tooltip would overflow top of viewport, show below instead
        if (top - tooltipRect.height < 4) {
            top = rect.bottom + 8;
            tooltip.style.transform = 'translate(-50%, 0)';
        }

        // Clamp horizontal position so tooltip doesn't overflow viewport edges
        const halfWidth = tooltipRect.width / 2;
        if (left - halfWidth < 4) left = halfWidth + 4;
        if (left + halfWidth > window.innerWidth - 4) left = window.innerWidth - halfWidth - 4;

        tooltip.style.left = left + 'px';
        tooltip.style.top = top + 'px';

        requestAnimationFrame(() => tooltip?.classList.add('visible'));

        activeBtn = btn;
    }

    function removeTooltip() {
        clearTimeout(dismissTimer);
        if (tooltip) {
            tooltip.remove();
            tooltip = null;
        }
        activeBtn = null;
    }

    function preventClick(e) {
        e.stopImmediatePropagation();
        e.preventDefault();
    }

    function onTouchStart(e) {
        const btn = e.target.closest(SELECTOR);
        if (!btn) return;

        clearTimeout(pressTimer);
        clearTimeout(dismissTimer);

        pressTimer = setTimeout(() => {
            showTooltip(btn);
            // Prevent the click from firing after long-press
            btn.addEventListener('click', preventClick, { once: true, capture: true });
        }, LONG_PRESS_MS);
    }

    function onTouchEnd() {
        clearTimeout(pressTimer);
        pressTimer = null;
        if (tooltip) {
            dismissTimer = setTimeout(removeTooltip, AUTO_DISMISS_MS);
        }
    }

    function init() {
        document.body.addEventListener('touchstart', onTouchStart, { passive: true });
        document.body.addEventListener('touchend', onTouchEnd, { passive: true });
        document.body.addEventListener('touchcancel', onTouchEnd, { passive: true });
    }

    function destroy() {
        document.body.removeEventListener('touchstart', onTouchStart);
        document.body.removeEventListener('touchend', onTouchEnd);
        document.body.removeEventListener('touchcancel', onTouchEnd);
        removeTooltip();
    }

    // Auto-init when DOM is ready
    if (document.readyState === 'loading') {
        document.addEventListener('DOMContentLoaded', init, { once: true });
    } else {
        init();
    }

    window.longPressTooltip = { init, destroy };
})();
