export interface Toast {
  id: number;
  message: string;
  tone: "error" | "success";
}

let nextId = 0;

class ToastStore {
  items = $state<Toast[]>([]);

  push(message: string, tone: Toast["tone"] = "error"): void {
    const id = nextId++;
    this.items = [...this.items, { id, message, tone }];
    setTimeout(() => this.dismiss(id), 6000);
  }

  dismiss(id: number): void {
    this.items = this.items.filter((toast) => toast.id !== id);
  }
}

export const toasts = new ToastStore();
