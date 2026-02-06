// Macro Editor
class MacroEditor {
    constructor() {
        this.currentMacro = null;
    }

    show(macro = null) {
        this.currentMacro = macro;
        const isEdit = !!macro;

        const content = `
            <form id="macro-form">
                <div class="form-group">
                    <label class="form-label">Name</label>
                    <input type="text" class="form-input" name="name" value="${macro?.name || ''}" required ${macro?.is_builtin ? 'disabled' : ''}>
                </div>
                <div class="form-group">
                    <label class="form-label">Description</label>
                    <input type="text" class="form-input" name="description" value="${macro?.description || ''}">
                </div>
                <div class="form-group">
                    <label class="form-label">Target Type</label>
                    <select class="form-select" name="target_type" ${macro?.is_builtin ? 'disabled' : ''}>
                        <option value="any" ${macro?.target_type === 'any' ? 'selected' : ''}>Any</option>
                        <option value="local" ${macro?.target_type === 'local' ? 'selected' : ''}>Local Only</option>
                        <option value="remote" ${macro?.target_type === 'remote' ? 'selected' : ''}>Remote Only</option>
                    </select>
                </div>
                <div class="form-group">
                    <label class="form-label">Script</label>
                    <textarea class="form-textarea" name="script" rows="15" style="font-family: monospace;" required ${macro?.is_builtin ? 'disabled' : ''}>${this.escapeHtml(macro?.script || '#!/bin/bash\n')}</textarea>
                </div>
            </form>
        `;

        let actions = `<button class="btn btn-secondary" onclick="app.hideModal()">Cancel</button>`;

        if (!macro?.is_builtin) {
            if (isEdit) {
                actions += `<button class="btn btn-danger" onclick="macroEditor.delete()">Delete</button>`;
            }
            actions += `<button class="btn btn-primary" onclick="macroEditor.save()">${isEdit ? 'Save' : 'Create'}</button>`;
        }

        app.showModal(isEdit ? 'Edit Macro' : 'New Macro', content, actions);
    }

    async save() {
        const form = document.getElementById('macro-form');
        const data = {
            name: form.querySelector('input[name="name"]').value,
            description: form.querySelector('input[name="description"]').value,
            target_type: form.querySelector('select[name="target_type"]').value,
            script: form.querySelector('textarea[name="script"]').value
        };

        try {
            if (this.currentMacro?.id) {
                await app.api('PUT', `/macros/${this.currentMacro.id}`, data);
            } else {
                await app.api('POST', '/macros', data);
            }
            app.hideModal();
            app.showToast('Success', 'Macro saved', 'success');
            app.loadMacros();
        } catch (error) {
            app.showToast('Error', error.message, 'error');
        }
    }

    async delete() {
        if (!this.currentMacro?.id) return;
        if (!confirm('Delete this macro?')) return;

        try {
            await app.api('DELETE', `/macros/${this.currentMacro.id}`);
            app.hideModal();
            app.showToast('Success', 'Macro deleted', 'success');
            app.loadMacros();
        } catch (error) {
            app.showToast('Error', error.message, 'error');
        }
    }

    escapeHtml(text) {
        const div = document.createElement('div');
        div.textContent = text || '';
        return div.innerHTML;
    }
}

// Initialize macro editor
window.macroEditor = new MacroEditor();
