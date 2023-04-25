import { vectorStore, handler } from './config';
import { Handler, WebHandler } from './handler';
import { PrismaStore, HNSWLibStore, Store } from './store';

function getVectorStore(): Store {
    switch (vectorStore) {
        case "prisma":
            return new PrismaStore();
        case "hnswlib":
            return new HNSWLibStore();
        default:
            throw new Error("Invalid vector store");
    }
}

function getHandler(): Handler {
    const store = getVectorStore();

    switch (handler) {
        case "web":
            return new WebHandler(store);
        default:
            throw new Error("Invalid prompt handler");
    }
}

(async () => {
    const handler = getHandler();
    await handler.handle();
})();