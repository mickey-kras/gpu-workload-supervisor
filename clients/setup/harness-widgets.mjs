export function createWidgetClass(widgets) {
    class Widget {
        signals = new Map(); children = []; sensitive = true;
        constructor(properties = {}) {
            for (const [property, signal] of [['text', 'changed'], ['active', 'toggled'], ['selected', 'notify::selected']]) {
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
        connect(signal, fn) { this.signals.set(signal, fn); }
        emit(signal) { return this.signals.get(signal)?.(this); }
        append(child) { this.children.push(child); }
        add(child) { this.append(child); }
        add_row(child) { this.append(child); }
        add_top_bar(child) { this.append(child); }
        add_bottom_bar(child) { this.append(child); }
        set_child(child) { this.append(child); }
        set_content(child) { this.append(child); }
        add_css_class(name) { this.cssClasses ??= []; this.cssClasses.push(name); }
        remove(child) {
            const index = this.children.indexOf(child);
            if (index < 0) throw new Error('Cannot remove a widget that is not a direct child');
            this.children.splice(index, 1);
        }
        present() { this.presented = true; }
        close() { this.closed = true; }
        grab_focus() { this.focused = true; }
        run() { this.emit('activate'); }
    }
    return Widget;
}
