import inquirer from "inquirer";
import ora from "ora";
import { Store } from "../store";
import { Responder } from "../responder";

export abstract class Handler {
    constructor(protected store: Store, protected responder: Responder) { }

    async handle() {
        const respond = this.responder.respond(true);

        while (true) {
            const { question } = await inquirer.prompt([
                {
                    type: 'input',
                    name: 'question',
                    message: 'Enter question: ',
                    validate: async (input: string) => {
                        if (input.length === 0) {
                            return 'Please enter a question';
                        }
                        return true;
                    }
                },
            ]);

            const spinner = ora("Thinking...").start();
            await respond(question, (token) => {
                spinner.stop();
                process.stdout.write(token);
            });
            process.stdout.write("\n");
        }
    }
}

import WebHandler from "./web";
export { WebHandler };