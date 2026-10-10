export function createWidgetClass(widgets) {
    class Widget {
        signals = new Map(); children = []; sensitive = true;
        constructor(properties = {}) {
            for (const [property, signal] of [['label', 'notify::label'], ['text', 'changed'], ['active', 'toggled'], ['selected', 'notify::selected']]) {
                Object.defineProperty(this, property, {
                    get: () => this[`_${property}`],
                    set: value => {
                        if (this[`_${property}`] === value) return;
                        this[`_${property}`] = value; this.emit(signal);
                    },
                });
            }
            Object.assign(this, properties); widgets.push(this);
        }
        update_property(properties, values) { this.accessibleProperties = Object.fromEntries(properties.map((property, index) => [property, values[index]])); }
        update_state(states, values) { this.accessibleStates = Object.fromEntries(states.map((state, index) => [state, values[index]])); }
        connect(signal, fn) { this.signals.set(signal, fn); }
        emit(signal) { return this.signals.get(signal)?.(this); }
        append(child) { this.children.push(child); }
        add(child) { this.append(child); }
        add_row(child) { this.append(child); }
        pack_end(child) { this.append(child); }
        pack_start(child) { this.append(child); }
        set_title_widget(child) {
            if (this.titleWidget) this.remove(this.titleWidget);
            this.titleWidget = child;
            if (child) this.append(child);
        }
        add_top_bar(child) { this.append(child); }
        add_bottom_bar(child) { this.append(child); }
        set_child(child) { this.append(child); }
        set_content(child) { this.append(child); }
        add_css_class(name) { this.cssClasses ??= []; this.cssClasses.push(name); }
        get_display() { return {get_clipboard: () => ({set_content: provider => { this.clipboard = provider.value.text; }})}; }
        get_scale_factor() { return 1; }
        get_vadjustment() { this.adjustment ??= {value: 0}; return this.adjustment; }
        remove_css_class(name) { this.cssClasses = this.cssClasses?.filter(item => item !== name); }
        remove(child) {
            const index = this.children.indexOf(child);
            if (index < 0) throw new Error('Cannot remove a widget that is not a direct child');
            this.children.splice(index, 1);
        }
        present() { this.presented = true; this.emit('map'); }
        close() {
            if (this.emit('close-request') === true) return;
            this.closed = true; this.emit('unrealize');
        }
        grab_focus() { this.focused = true; }
        select_region(start, end) { this.selection = [start, end]; }
        run() { this.emit('activate'); }
    }
    return Widget;
}
